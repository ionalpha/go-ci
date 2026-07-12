package release

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Sign produces a keyless cosign signature over the checksum file and returns the
// detached signature and certificate as artifacts.
//
// Keyless means there is no long-lived private key to hold, rotate or leak: cosign
// exchanges the CI job's OIDC token for a short-lived Sigstore certificate that names
// the workflow identity that ran, and that identity is what a verifier pins. So the
// question a consumer can answer is not "does someone hold the key" but "was this built
// by that workflow, in that repo, at that tag".
//
// Only checksums.txt is signed. It commits to every archive by digest, so one signature
// covers the whole release, and there is exactly one thing for a verifier to check.
func Sign(ctx context.Context, checksums Artifact) ([]Artifact, error) {
	if _, err := exec.LookPath("cosign"); err != nil {
		return nil, fmt.Errorf("cosign is not installed: %w", err)
	}
	sigPath := checksums.Path + ".sig"
	certPath := checksums.Path + ".pem"

	// cosign 3 defaults to the bundle format and ignores these flags; the workflow pins
	// cosign 2.x, and this checks that the files it promised actually appeared rather
	// than publishing a release whose signature silently is not there.
	cmd := exec.CommandContext(ctx, "cosign", "sign-blob", "--yes", //nolint:gosec // G204: paths this tool composed, passed as argv
		"--output-signature="+sigPath,
		"--output-certificate="+certPath,
		checksums.Path,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("cosign sign-blob failed (exit %d); keyless signing needs an OIDC token, so it only works in CI with id-token: write", ee.ExitCode())
		}
		return nil, fmt.Errorf("cosign sign-blob: %w", err)
	}
	for _, p := range []string{sigPath, certPath} {
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			return nil, fmt.Errorf("cosign reported success but %s is missing or empty; is cosign 2.x installed?", filepath.Base(p))
		}
	}
	return []Artifact{
		{Name: filepath.Base(sigPath), Path: sigPath, Kind: KindSignature},
		{Name: filepath.Base(certPath), Path: certPath, Kind: KindCertificate},
	}, nil
}

// SBOM writes a CycloneDX SBOM beside each archive using syft, and returns them as
// artifacts. The SBOMs are hashed into checksums.txt like any other artifact, so the
// release's single signature covers them too.
func SBOM(ctx context.Context, archives []Artifact) ([]Artifact, error) {
	if _, err := exec.LookPath("syft"); err != nil {
		return nil, fmt.Errorf("syft is not installed: %w", err)
	}
	var out []Artifact
	for _, a := range archives {
		dst := a.Path + ".sbom.json"
		cmd := exec.CommandContext(ctx, "syft", "scan", "file:"+a.Path, //nolint:gosec // G204: an artifact path this tool just wrote
			"-o", "cyclonedx-json="+dst, "-q")
		// syft stamps a serial number and timestamp unless told the source date, which
		// would make the SBOM differ on every run of the same commit.
		cmd.Env = append(os.Environ(), "SYFT_FORMAT_CYCLONEDX_JSON_DETERMINISTIC=true")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("syft scan %s: %w", a.Name, err)
		}
		out = append(out, Artifact{Name: filepath.Base(dst), Path: dst, Platform: a.Platform, Kind: KindSBOM})
	}
	return out, nil
}

// CertificateIdentity extracts the signing identity Sigstore actually bound into the
// certificate: the SAN URI, which is the workflow ref that requested the signature.
//
// It is read back out of the issued certificate rather than assembled from the tag and
// repo, because guessing it wrong is easy and silently breaks every verifier. When a
// release runs through a *reusable* workflow, Fulcio binds the identity of the reusable
// workflow (job_workflow_ref), not the caller's release.yml, so the intuitive guess is
// the wrong one. Reading it from the cert cannot drift.
func CertificateIdentity(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath) //nolint:gosec // G304: the certificate cosign just emitted
	if err != nil {
		return "", err
	}
	// cosign writes the certificate base64-encoded; tolerate both that and raw PEM.
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err == nil {
		raw = dec
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return "", fmt.Errorf("%s: not a PEM certificate", filepath.Base(certPath))
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(certPath), err)
	}
	if len(cert.URIs) == 0 {
		return "", fmt.Errorf("%s: certificate carries no SAN URI identity", filepath.Base(certPath))
	}
	return cert.URIs[0].String(), nil
}

// OIDCIssuer is the issuer Sigstore records for a signature minted from a GitHub Actions
// OIDC token. A verifier must pin it alongside the identity; without it, an identity
// string from some other issuer would satisfy the check.
const OIDCIssuer = "https://token.actions.githubusercontent.com"

// VerifyCommand is the exact cosign invocation a consumer runs to verify a release,
// rendered into the release notes. The identity comes from the certificate that was just
// issued, so the published instructions always match the signature that was actually made.
func VerifyCommand(identity string) string {
	return strings.Join([]string{
		"cosign verify-blob checksums.txt \\",
		"  --signature checksums.txt.sig \\",
		"  --certificate checksums.txt.pem \\",
		fmt.Sprintf("  --certificate-identity %q \\", identity),
		fmt.Sprintf("  --certificate-oidc-issuer %q", OIDCIssuer),
	}, "\n")
}
