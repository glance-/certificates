package cloudcas

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"regexp"
	"strings"
	"time"

	privateca "cloud.google.com/go/security/privateca/apiv1beta1"
	"github.com/google/uuid"
	gax "github.com/googleapis/gax-go/v2"
	"github.com/pkg/errors"
	"github.com/smallstep/certificates/cas/apiv1"
	"google.golang.org/api/option"
	pb "google.golang.org/genproto/googleapis/cloud/security/privateca/v1beta1"
	durationpb "google.golang.org/protobuf/types/known/durationpb"
)

func init() {
	apiv1.Register(apiv1.CloudCAS, func(ctx context.Context, opts apiv1.Options) (apiv1.CertificateAuthorityService, error) {
		return New(ctx, opts)
	})
}

// The actual regular expression that matches a certificate authority is:
//   ^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/locations/[a-z0-9-]+/certificateAuthorities/[a-zA-Z0-9-_]+$
// But we will allow a more flexible one to fail if this changes.
var caRegexp = regexp.MustCompile("^projects/[^/]+/locations/[^/]+/certificateAuthorities/[^/]+$")

// CertificateAuthorityClient is the interface implemented by the Google CAS
// client.
type CertificateAuthorityClient interface {
	CreateCertificate(ctx context.Context, req *pb.CreateCertificateRequest, opts ...gax.CallOption) (*pb.Certificate, error)
	RevokeCertificate(ctx context.Context, req *pb.RevokeCertificateRequest, opts ...gax.CallOption) (*pb.Certificate, error)
	GetCertificateAuthority(ctx context.Context, req *pb.GetCertificateAuthorityRequest, opts ...gax.CallOption) (*pb.CertificateAuthority, error)
	CreateCertificateAuthority(ctx context.Context, req *pb.CreateCertificateAuthorityRequest, opts ...gax.CallOption) (*privateca.CreateCertificateAuthorityOperation, error)
	FetchCertificateAuthorityCsr(ctx context.Context, req *pb.FetchCertificateAuthorityCsrRequest, opts ...gax.CallOption) (*pb.FetchCertificateAuthorityCsrResponse, error)
	ActivateCertificateAuthority(ctx context.Context, req *pb.ActivateCertificateAuthorityRequest, opts ...gax.CallOption) (*privateca.ActivateCertificateAuthorityOperation, error)
}

// recocationCodeMap maps revocation reason codes from RFC 5280, to Google CAS
// revocation reasons. Revocation reason 7 is not used, and revocation reason 8
// (removeFromCRL) is not supported by Google CAS.
var revocationCodeMap = map[int]pb.RevocationReason{
	0:  pb.RevocationReason_REVOCATION_REASON_UNSPECIFIED,
	1:  pb.RevocationReason_KEY_COMPROMISE,
	2:  pb.RevocationReason_CERTIFICATE_AUTHORITY_COMPROMISE,
	3:  pb.RevocationReason_AFFILIATION_CHANGED,
	4:  pb.RevocationReason_SUPERSEDED,
	5:  pb.RevocationReason_CESSATION_OF_OPERATION,
	6:  pb.RevocationReason_CERTIFICATE_HOLD,
	9:  pb.RevocationReason_PRIVILEGE_WITHDRAWN,
	10: pb.RevocationReason_ATTRIBUTE_AUTHORITY_COMPROMISE,
}

// CloudCAS implements a Certificate Authority Service using Google Cloud CAS.
type CloudCAS struct {
	client               CertificateAuthorityClient
	certificateAuthority string
	project              string
	location             string
}

// newCertificateAuthorityClient creates the certificate authority client. This
// function is used for testing purposes.
var newCertificateAuthorityClient = func(ctx context.Context, credentialsFile string) (CertificateAuthorityClient, error) {
	var cloudOpts []option.ClientOption
	if credentialsFile != "" {
		cloudOpts = append(cloudOpts, option.WithCredentialsFile(credentialsFile))
	}
	client, err := privateca.NewCertificateAuthorityClient(ctx, cloudOpts...)
	if err != nil {
		return nil, errors.Wrap(err, "error creating client")
	}
	return client, nil
}

// New creates a new CertificateAuthorityService implementation using Google
// Cloud CAS.
func New(ctx context.Context, opts apiv1.Options) (*CloudCAS, error) {
	if opts.IsCreator {
		switch {
		case opts.Project == "":
			return nil, errors.New("cloudCAS 'project' cannot be empty")
		case opts.Location == "":
			return nil, errors.New("cloudCAS 'location' cannot be empty")
		}
	} else {
		if opts.CertificateAuthority == "" {
			return nil, errors.New("cloudCAS 'certificateAuthority' cannot be empty")
		}
		if !caRegexp.MatchString(opts.CertificateAuthority) {
			return nil, errors.New("cloudCAS 'certificateAuthority' is not valid certificate authority resource")
		}
		// Extract project and location from CertificateAuthority
		if parts := strings.Split(opts.CertificateAuthority, "/"); len(parts) == 6 {
			if opts.Project == "" {
				opts.Project = parts[1]
			}
			if opts.Location == "" {
				opts.Location = parts[3]
			}
		}
	}

	client, err := newCertificateAuthorityClient(ctx, opts.CredentialsFile)
	if err != nil {
		return nil, err
	}

	return &CloudCAS{
		client:               client,
		certificateAuthority: opts.CertificateAuthority,
		project:              opts.Project,
		location:             opts.Location,
	}, nil
}

// GetCertificateAuthority returns the root certificate for the given
// certificate authority. It implements apiv1.CertificateAuthorityGetter
// interface.
func (c *CloudCAS) GetCertificateAuthority(req *apiv1.GetCertificateAuthorityRequest) (*apiv1.GetCertificateAuthorityResponse, error) {
	name := req.Name
	if name == "" {
		name = c.certificateAuthority
	}

	ctx, cancel := defaultContext()
	defer cancel()

	resp, err := c.client.GetCertificateAuthority(ctx, &pb.GetCertificateAuthorityRequest{
		Name: name,
	})
	if err != nil {
		println(name)
		return nil, errors.Wrap(err, "cloudCAS GetCertificateAuthority failed")
	}
	if len(resp.PemCaCertificates) == 0 {
		return nil, errors.New("cloudCAS GetCertificateAuthority: PemCACertificate should not be empty")
	}

	// Last certificate in the chain is the root.
	root, err := parseCertificate(resp.PemCaCertificates[len(resp.PemCaCertificates)-1])
	if err != nil {
		return nil, err
	}

	return &apiv1.GetCertificateAuthorityResponse{
		RootCertificate: root,
	}, nil
}

// CreateCertificate signs a new certificate using Google Cloud CAS.
func (c *CloudCAS) CreateCertificate(req *apiv1.CreateCertificateRequest) (*apiv1.CreateCertificateResponse, error) {
	switch {
	case req.Template == nil:
		return nil, errors.New("createCertificateRequest `template` cannot be nil")
	case req.Lifetime == 0:
		return nil, errors.New("createCertificateRequest `lifetime` cannot be 0")
	}

	cert, chain, err := c.createCertificate(req.Template, req.Lifetime, req.RequestID)
	if err != nil {
		return nil, err
	}

	return &apiv1.CreateCertificateResponse{
		Certificate:      cert,
		CertificateChain: chain,
	}, nil
}

// RenewCertificate renews the given certificate using Google Cloud CAS.
// Google's CAS does not support the renew operation, so this method uses
// CreateCertificate.
func (c *CloudCAS) RenewCertificate(req *apiv1.RenewCertificateRequest) (*apiv1.RenewCertificateResponse, error) {
	switch {
	case req.Template == nil:
		return nil, errors.New("renewCertificateRequest `template` cannot be nil")
	case req.Lifetime == 0:
		return nil, errors.New("renewCertificateRequest `lifetime` cannot be 0")
	}

	cert, chain, err := c.createCertificate(req.Template, req.Lifetime, req.RequestID)
	if err != nil {
		return nil, err
	}

	return &apiv1.RenewCertificateResponse{
		Certificate:      cert,
		CertificateChain: chain,
	}, nil
}

// RevokeCertificate revokes a certificate using Google Cloud CAS.
func (c *CloudCAS) RevokeCertificate(req *apiv1.RevokeCertificateRequest) (*apiv1.RevokeCertificateResponse, error) {
	reason, ok := revocationCodeMap[req.ReasonCode]
	switch {
	case !ok:
		return nil, errors.Errorf("revokeCertificate 'reasonCode=%d' is invalid or not supported", req.ReasonCode)
	case req.Certificate == nil:
		return nil, errors.New("revokeCertificateRequest `certificate` cannot be nil")
	}

	ext, ok := apiv1.FindCertificateAuthorityExtension(req.Certificate)
	if !ok {
		return nil, errors.New("error revoking certificate: certificate authority extension was not found")
	}

	var cae apiv1.CertificateAuthorityExtension
	if _, err := asn1.Unmarshal(ext.Value, &cae); err != nil {
		return nil, errors.Wrap(err, "error unmarshaling certificate authority extension")
	}

	ctx, cancel := defaultContext()
	defer cancel()

	certpb, err := c.client.RevokeCertificate(ctx, &pb.RevokeCertificateRequest{
		Name:      c.certificateAuthority + "/certificates/" + cae.CertificateID,
		Reason:    reason,
		RequestId: req.RequestID,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cloudCAS RevokeCertificate failed")
	}

	cert, chain, err := getCertificateAndChain(certpb)
	if err != nil {
		return nil, err
	}

	return &apiv1.RevokeCertificateResponse{
		Certificate:      cert,
		CertificateChain: chain,
	}, nil
}

// CreateCertificateAuthority creates a new root or intermediate certificate
// using Google Cloud CAS.
func (c *CloudCAS) CreateCertificateAuthority(req *apiv1.CreateCertificateAuthorityRequest) (*apiv1.CreateCertificateAuthorityResponse, error) {
	switch {
	case c.project == "":
		return nil, errors.New("cloudCAS `project` cannot be empty")
	case c.location == "":
		return nil, errors.New("cloudCAS `location` cannot be empty")
	case req.Template == nil:
		return nil, errors.New("createCertificateAuthorityRequest `template` cannot be nil")
	case req.Lifetime == 0:
		return nil, errors.New("createCertificateAuthorityRequest `lifetime` cannot be 0")
	case req.Type == apiv1.IntermediateCA && req.Parent == nil:
		return nil, errors.New("createCertificateAuthorityRequest `parent` cannot be nil")
	case req.Type == apiv1.IntermediateCA && req.Parent.Name == "":
		return nil, errors.New("createCertificateAuthorityRequest `parent.name` cannot be empty")
	}

	// Select key and signature algorithm to use
	var err error
	var keySpec *pb.CertificateAuthority_KeyVersionSpec
	if req.CreateKey == nil {
		if keySpec, err = createKeyVersionSpec(0, 0); err != nil {
			return nil, errors.Wrap(err, "createCertificateAuthorityRequest `createKey` is not valid")
		}
	} else {
		if keySpec, err = createKeyVersionSpec(req.CreateKey.SignatureAlgorithm, req.CreateKey.Bits); err != nil {
			return nil, errors.Wrap(err, "createCertificateAuthorityRequest `createKey` is not valid")
		}
	}

	// Normalize or generate id.
	certificateAuthorityID := normalizeCertificateAuthorityName(req.Name)
	if certificateAuthorityID == "" {
		id, err := createCertificateID()
		if err != nil {
			return nil, err
		}
		certificateAuthorityID = id
	}

	// Add CertificateAuthority extension
	casExtension, err := apiv1.CreateCertificateAuthorityExtension(apiv1.CloudCAS, certificateAuthorityID)
	if err != nil {
		return nil, err
	}
	req.Template.ExtraExtensions = append(req.Template.ExtraExtensions, casExtension)

	// Prepare CreateCertificateAuthorityRequest
	pbReq := &pb.CreateCertificateAuthorityRequest{
		Parent:                 "projects/" + c.project + "/locations/" + c.location,
		CertificateAuthorityId: certificateAuthorityID,
		RequestId:              req.RequestID,
		CertificateAuthority: &pb.CertificateAuthority{
			Type: pb.CertificateAuthority_TYPE_UNSPECIFIED,
			Tier: pb.CertificateAuthority_ENTERPRISE,
			Config: &pb.CertificateConfig{
				SubjectConfig: &pb.CertificateConfig_SubjectConfig{
					Subject:    createSubject(req.Template),
					CommonName: req.Template.Subject.CommonName,
				},
				ReusableConfig: createReusableConfig(req.Template),
			},
			Lifetime: durationpb.New(req.Lifetime),
			KeySpec:  keySpec,
			IssuingOptions: &pb.CertificateAuthority_IssuingOptions{
				IncludeCaCertUrl:    true,
				IncludeCrlAccessUrl: true,
			},
			Labels: map[string]string{},
		},
	}

	switch req.Type {
	case apiv1.RootCA:
		pbReq.CertificateAuthority.Type = pb.CertificateAuthority_SELF_SIGNED
	case apiv1.IntermediateCA:
		pbReq.CertificateAuthority.Type = pb.CertificateAuthority_SUBORDINATE
	default:
		return nil, errors.Errorf("createCertificateAuthorityRequest `type=%d' is invalid or not supported", req.Type)
	}

	// Create certificate authority.
	ctx, cancel := defaultContext()
	defer cancel()

	resp, err := c.client.CreateCertificateAuthority(ctx, pbReq)
	if err != nil {
		return nil, errors.Wrap(err, "cloudCAS CreateCertificateAuthority failed")
	}

	// Wait for the long-running operation.
	ctx, cancel = defaultInitiatorContext()
	defer cancel()

	ca, err := resp.Wait(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "cloudCAS CreateCertificateAuthority failed")
	}

	// Sign Intermediate CAs with the parent.
	if req.Type == apiv1.IntermediateCA {
		ca, err = c.signIntermediateCA(ca.Name, req)
		if err != nil {
			return nil, err
		}
	}

	if len(ca.PemCaCertificates) == 0 {
		return nil, errors.New("cloudCAS CreateCertificateAuthority failed: PemCaCertificates is empty")
	}

	cert, err := parseCertificate(ca.PemCaCertificates[0])
	if err != nil {
		return nil, err
	}

	var chain []*x509.Certificate
	if pemChain := ca.PemCaCertificates[1:]; len(pemChain) > 0 {
		chain = make([]*x509.Certificate, len(pemChain))
		for i, s := range pemChain {
			if chain[i], err = parseCertificate(s); err != nil {
				return nil, err
			}
		}
	}

	return &apiv1.CreateCertificateAuthorityResponse{
		Name:             ca.Name,
		Certificate:      cert,
		CertificateChain: chain,
	}, nil
}

func (c *CloudCAS) createCertificate(tpl *x509.Certificate, lifetime time.Duration, requestID string) (*x509.Certificate, []*x509.Certificate, error) {
	// Removes the CAS extension if it exists.
	apiv1.RemoveCertificateAuthorityExtension(tpl)

	// Create new CAS extension with the certificate id.
	id, err := createCertificateID()
	if err != nil {
		return nil, nil, err
	}
	casExtension, err := apiv1.CreateCertificateAuthorityExtension(apiv1.CloudCAS, id)
	if err != nil {
		return nil, nil, err
	}
	tpl.ExtraExtensions = append(tpl.ExtraExtensions, casExtension)

	// Create and submit certificate
	certConfig, err := createCertificateConfig(tpl)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := defaultContext()
	defer cancel()

	cert, err := c.client.CreateCertificate(ctx, &pb.CreateCertificateRequest{
		Parent:        c.certificateAuthority,
		CertificateId: id,
		Certificate: &pb.Certificate{
			CertificateConfig: certConfig,
			Lifetime:          durationpb.New(lifetime),
			Labels:            map[string]string{},
		},
		RequestId: requestID,
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "cloudCAS CreateCertificate failed")
	}

	// Return certificate and certificate chain
	return getCertificateAndChain(cert)
}

func (c *CloudCAS) signIntermediateCA(name string, req *apiv1.CreateCertificateAuthorityRequest) (*pb.CertificateAuthority, error) {
	id, err := createCertificateID()
	if err != nil {
		return nil, err
	}

	// Fetch intermediate CSR
	ctx, cancel := defaultInitiatorContext()
	defer cancel()

	csr, err := c.client.FetchCertificateAuthorityCsr(ctx, &pb.FetchCertificateAuthorityCsrRequest{
		Name: name,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cloudCAS FetchCertificateAuthorityCsr failed")
	}

	// Sign the CSR with the ca.
	ctx, cancel = defaultInitiatorContext()
	defer cancel()

	sign, err := c.client.CreateCertificate(ctx, &pb.CreateCertificateRequest{
		Parent:        req.Parent.Name,
		CertificateId: id,
		Certificate: &pb.Certificate{
			// Name: "projects/" + c.project + "/locations/" + c.location + "/certificates/" + id,
			CertificateConfig: &pb.Certificate_PemCsr{
				PemCsr: csr.PemCsr,
			},
			Lifetime: durationpb.New(req.Lifetime),
			Labels:   map[string]string{},
		},
		RequestId: req.RequestID,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cloudCAS CreateCertificate failed")
	}

	ctx, cancel = defaultInitiatorContext()
	defer cancel()
	resp, err := c.client.ActivateCertificateAuthority(ctx, &pb.ActivateCertificateAuthorityRequest{
		Name:             name,
		PemCaCertificate: sign.PemCertificate,
		SubordinateConfig: &pb.SubordinateConfig{
			SubordinateConfig: &pb.SubordinateConfig_PemIssuerChain{
				PemIssuerChain: &pb.SubordinateConfig_SubordinateConfigChain{
					PemCertificates: sign.PemCertificateChain,
				},
			},
		},
		RequestId: req.RequestID,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cloudCAS ActivateCertificateAuthority1 failed")
	}

	// Wait for the long-running operation.
	ctx, cancel = defaultInitiatorContext()
	defer cancel()

	ca, err := resp.Wait(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "cloudCAS ActivateCertificateAuthority failed")
	}

	return ca, nil
}

func defaultContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func defaultInitiatorContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func createCertificateID() (string, error) {
	id, err := uuid.NewRandomFromReader(rand.Reader)
	if err != nil {
		return "", errors.Wrap(err, "error creating certificate id")
	}
	return id.String(), nil
}

func parseCertificate(pemCert string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemCert))
	if block == nil {
		return nil, errors.New("error decoding certificate: not a valid PEM encoded block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing certificate")
	}
	return cert, nil
}

func getCertificateAndChain(certpb *pb.Certificate) (*x509.Certificate, []*x509.Certificate, error) {
	cert, err := parseCertificate(certpb.PemCertificate)
	if err != nil {
		return nil, nil, err
	}

	pemChain := certpb.PemCertificateChain[:len(certpb.PemCertificateChain)-1]
	chain := make([]*x509.Certificate, len(pemChain))
	for i := range pemChain {
		chain[i], err = parseCertificate(pemChain[i])
		if err != nil {
			return nil, nil, err
		}
	}

	return cert, chain, nil

}

// Normalize a certificate authority name to comply with [a-zA-Z0-9-_].
func normalizeCertificateAuthorityName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-':
			return r
		case r == '_':
			return r
		default:
			return '-'
		}
	}, name)
}
