// Copyright 2021-2024 Zenauth Ltd.
// SPDX-License-Identifier: Apache-2.0

// Package reimage provides tools for processing/updating the images listed in k8s manifests
package reimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	grafeas "cloud.google.com/go/grafeas/apiv1"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/googleapis/gax-go/v2"

	"google.golang.org/api/iterator"
	grafeaspb "google.golang.org/genproto/googleapis/grafeas/v1"
)

// GrafeasClient still isn't mockable, need to wrap it
type GrafeasClient interface {
	ListOccurrences(ctx context.Context, req *grafeaspb.ListOccurrencesRequest, opts ...gax.CallOption) *grafeas.OccurrenceIterator
	CreateOccurrence(ctx context.Context, req *grafeaspb.CreateOccurrenceRequest, opts ...gax.CallOption) (*grafeaspb.Occurrence, error)
}

// GrafeasVulnGetter checks that images have been scanned, and checks that
// they do not contain unexpected vulnerabilities
type GrafeasVulnGetter struct {
	Grafeas GrafeasClient
	Logger
	Parent     string
	RetryMax   int
	RetryDelay time.Duration
}

func (vc *GrafeasVulnGetter) getDiscovery(ctx context.Context, dig name.Digest) (*grafeaspb.DiscoveryOccurrence, error) {
	kind := grafeaspb.NoteKind_DISCOVERY
	req := &grafeaspb.ListOccurrencesRequest{
		Parent: vc.Parent,
		Filter: fmt.Sprintf(`((kind = "%s") AND (resourceUrl = "https://%s"))`, kind, dig),
	}
	occs := vc.Grafeas.ListOccurrences(ctx, req)
	for {
		occ, err := occs.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if occ.GetKind() == kind {
			return occ.GetDiscovery(), nil
		}
	}

	return nil, ErrDiscoveryNotFound
}

func (vc *GrafeasVulnGetter) getVulnerabilities(ctx context.Context, dig name.Digest) ([]*grafeaspb.VulnerabilityOccurrence, error) {
	req := &grafeaspb.ListOccurrencesRequest{
		Parent: vc.Parent,
		Filter: fmt.Sprintf(`((kind = "VULNERABILITY") AND (resourceUrl = "https://%s"))`, dig),
	}
	occs := vc.Grafeas.ListOccurrences(ctx, req)
	var res []*grafeaspb.VulnerabilityOccurrence
	for {
		occ, err := occs.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if occ.GetKind() == grafeaspb.NoteKind_VULNERABILITY {
			res = append(res, occ.GetVulnerability())
		}
	}

	return res, nil
}

// Check checks an individual image.
func (vc *GrafeasVulnGetter) check(ctx context.Context, dig name.Digest) ([]ImageVulnerability, error) {
	disc, err := vc.getDiscovery(ctx, dig)
	if err != nil {
		return nil, err
	}
	switch disc.AnalysisStatus {
	case grafeaspb.DiscoveryOccurrence_FINISHED_UNSUPPORTED:
		return nil, nil
	case grafeaspb.DiscoveryOccurrence_FINISHED_SUCCESS:
	default:
		return nil, ErrDiscoverNotFinished
	}

	voccs, err := vc.getVulnerabilities(ctx, dig)
	if err != nil {
		return nil, err
	}

	var res []ImageVulnerability

	for _, vocc := range voccs {
		score := vocc.GetCvssScore()
		cve := vocc.GetShortDescription()
		res = append(res, ImageVulnerability{
			ID:   cve,
			CVSS: score,
		})
	}

	return res, nil
}

// GetVulnerabilities waits for a completed vulnerability discovery, and then check that an image
// has no CVEs that violate the configured policy
func (vc *GrafeasVulnGetter) GetVulnerabilities(ctx context.Context, dig name.Digest) ([]ImageVulnerability, error) {
	var err error
	img := dig.String()

	baseDelay := 500 * time.Millisecond
	for i := 0; i <= vc.RetryMax; i++ {
		var res []ImageVulnerability
		res, err = vc.check(ctx, dig)
		if err == nil {
			return res, nil
		}

		if !(errors.Is(err, ErrDiscoverNotFinished) || errors.Is(err, ErrDiscoveryNotFound)) {
			return nil, err
		}

		secRetry := math.Pow(2, float64(i))
		delay := time.Duration(secRetry) * baseDelay

		if vc.Logger != nil {
			vc.Logger.Info("retrying discovery due to error", slog.String("img", img), slog.Duration("delay", delay), slog.String("err", err.Error()))
		}

		time.Sleep(delay)
	}

	return nil, err
}

// GCPBinAuthzPayload is the mandated attestation note for
// signing Docker/OCI images for Google's Binauthz implementation
type GCPBinAuthzPayload struct {
	Critical struct {
		Identity struct {
			DockerReference string `json:"docker-reference"`
		} `json:"identitiy"`
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// GCPBinAuthzConcisePayload is a convenient wrapper around GCPBinAuthzPayload
// it with json.Marshal to a GCPBinAuthzPayload with correctly set Type
type GCPBinAuthzConcisePayload struct {
	DockerReference      string
	DockerManifestDigest string
}

// MarshalJSON marshals the provided type to JSON, but conforming
// to the structure of a GCPBinAuthzPayload
func (pl *GCPBinAuthzConcisePayload) MarshalJSON() ([]byte, error) {
	jpl := GCPBinAuthzPayload{}

	jpl.Critical.Identity.DockerReference = pl.DockerReference
	jpl.Critical.Image.DockerManifestDigest = pl.DockerManifestDigest
	jpl.Critical.Type = "Google cloud binauthz container signature"

	return json.Marshal(jpl)
}

// Keyer is an interface to a private key, for signing and verifying
// blobs
type Keyer interface {
	Sign(ctx context.Context, bs []byte) ([]byte, string, error)
	Verify(ctx context.Context, bs []byte, sig []byte) error
}

// GrafeasAttester implements attestation creation and checking using Grafaes
type GrafeasAttester struct {
	Grafeas GrafeasClient
	Keys    Keyer
	Logger
	Parent  string
	NoteRef string
}

// Get retrieves all the Attestation occurrences for the given image that use the provided
// noteRef (or all if noteRef is "")
func (t *GrafeasAttester) Get(ctx context.Context, dig name.Digest, noteRef string) ([]*grafeaspb.AttestationOccurrence, error) {
	kind := grafeaspb.NoteKind_ATTESTATION
	req := &grafeaspb.ListOccurrencesRequest{
		Parent: t.Parent,
		Filter: fmt.Sprintf(`((kind = "%s") AND (resourceUrl = "https://%s"))`, kind, dig),
	}

	var res []*grafeaspb.AttestationOccurrence
	occs := t.Grafeas.ListOccurrences(ctx, req)
	for {
		occ, err := occs.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if occ.GetKind() == kind {
			if noteRef != "" && occ.NoteName != noteRef {
				continue
			}
			att := occ.GetAttestation()
			sigs := att.GetSignatures()
			for i, s := range sigs {
				if t.Logger != nil {
					t.Logger.Debug("verify", "payload", att.SerializedPayload, "sig", s.Signature)
				}
				if err := t.Keys.Verify(ctx, att.SerializedPayload, s.Signature); err != nil {
					if t.Logger != nil {
						encsig := base64.StdEncoding.EncodeToString(s.Signature)
						t.Logger.Info("failed to verify attestation", "img", dig.String(), "sig_num", i, "payload", att.SerializedPayload, "sig", encsig, "err", err.Error())
					}
					continue
				}
				res = append(res, att)
			}
		}
	}

	if res == nil {
		return nil, ErrAttestationNotFound
	}
	return res, nil
}

// Check confirms that a correctly signed attestation for NoteRef exists for the image digest
func (t *GrafeasAttester) Check(ctx context.Context, dig name.Digest) (bool, error) {
	_, err := t.Get(ctx, dig, t.NoteRef)
	if err != nil && !errors.Is(err, ErrAttestationNotFound) {
		return false, err
	}

	return !errors.Is(err, ErrAttestationNotFound), nil
}

// Attest creates a NoteRef attestation for digest. It will skip this if one already exist
func (t *GrafeasAttester) Attest(ctx context.Context, dig name.Digest) error {
	ok, err := t.Check(ctx, dig)
	if err != nil {
		return err
	}

	if ok {
		if t.Logger != nil {
			t.Logger.Debug("image %s already attested", "img", dig.String())
		}
		return nil
	}

	payload := GCPBinAuthzConcisePayload{
		DockerReference:      dig.String(),
		DockerManifestDigest: dig.DigestStr(),
	}

	payloadBytes, err := json.Marshal(&payload)
	if err != nil {
		return err
	}

	sig, kid, err := t.Keys.Sign(ctx, payloadBytes)
	if err != nil {
		return err
	}

	occSig := &grafeaspb.Signature{
		Signature:   sig,
		PublicKeyId: kid,
	}

	occAtt := &grafeaspb.Occurrence_Attestation{
		Attestation: &grafeaspb.AttestationOccurrence{
			SerializedPayload: payloadBytes,
			Signatures:        []*grafeaspb.Signature{occSig},
		},
	}

	occReq := &grafeaspb.CreateOccurrenceRequest{
		Parent: t.Parent,
		Occurrence: &grafeaspb.Occurrence{
			NoteName:    t.NoteRef,
			ResourceUri: fmt.Sprintf("https://%s", dig),
			Details:     occAtt,
		},
	}

	_, err = t.Grafeas.CreateOccurrence(ctx, occReq)

	return err
}
