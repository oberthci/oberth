// Command oberth-release packages, verifies and publishes exact release bytes.
// Spec: docs/plans/c13-own-pipelines.md#owned-data-and-public-contract
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"os"
	"os/signal"
	"syscall"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/staticgit"
)

type options struct {
	publicKey                              string
	replaceKey                             bool
	dir, base, pin, chart, version, commit string
	origin, token, receipt, gitOut         string
	git                                    func(staticgit.Architecture) (staticgit.Record, error)
}
type refusal string

const (
	imageMissing        refusal = "Release image is not published."
	imagePrivate        refusal = "Release image is absent or private."
	imageRepositories   refusal = "Release image repositories are missing or invalid."
	registryResponse    refusal = "Registry image lookup failed."
	registryUnavailable refusal = "Release image registry could not be reached."
	signingKeyMismatch  refusal = "Signing key does not match the release pin."
	keyInvalid          refusal = "Release public key is invalid."
	keyReplace          refusal = "Release public key differs from the existing pin."
	keyPinInvalid       refusal = "Existing release public key pin is invalid."
	keyMarkers          refusal = "Fetcher release key markers are missing or repeated."
	failure             refusal = "Release operation failed."
	usage               refusal = "Release command is invalid."
	releaseToken        refusal = "Release token file is missing or empty."
	inputs              refusal = "Release input is missing or invalid."
	basePin             refusal = "Base image pin is missing."
	chartStamp          refusal = "Chart release placeholders are missing or repeated."
	baseMismatch        refusal = "Base image differs from its reviewed pins."
	binariesMismatch    refusal = "Image binaries differ from the shipped inventory."
	latestInvalid       refusal = "GitHub's latest release has an invalid tag."
	storeFailure        refusal = "GitHub refused the release request."
	tagMissing          refusal = "Release tag is not on GitHub."
	assetUnconfirmed    refusal = "Published release asset cannot be confirmed."
	immutable           refusal = "Published release bytes differ."
	notVerified         refusal = "Release has no matching clean verification."
	tokenShared         refusal = "Release token file is open to group or others."
	assetsDiffer        refusal = "Release draft holds other assets than the signed files."
	tokenInside         refusal = "Release token file is inside the release directory."
)

func (r refusal) Error() string { return string(r) }
func (r refusal) Remedy() string {
	switch r {
	case keyReplace, keyPinInvalid:
		return "Review the public key and rerun hack/release-pin.sh with --replace."
	}
	return map[refusal]string{
		imageMissing:        "Push the named image with the command in Detail, then retry publication.",
		imagePrivate:        "Push the image if it is absent, then allow public reads; see Detail.",
		imageRepositories:   "Set both image repositories in the chart, then rebuild and sign the release.",
		registryResponse:    "Read the registry response in Detail, resolve its cause, then retry verification.",
		registryUnavailable: "Check the registry address, network and TLS trust, then retry verification.",
		signingKeyMismatch:  "Use the private key for the committed release public key.",
		keyInvalid:          "Use a PEM-encoded P-256 public key; keep its private key outside the tree.",
		keyMarkers:          "Restore exactly one public-key block in the fetcher.",
		failure:             "Read the native error detail, fix the reported problem and retry.",
		usage:               "Use base, package, key, pin, signature, verify, publish or finalize with required flags.",
		releaseToken:        "Pass --token-file with a GitHub token that may write this repository's releases.",
		inputs:              "Provide the commands, chart, base layout and exact release version.",
		basePin:             "Review the displayed base digest and write the canonical base-image pin.",
		baseMismatch:        "Prepare the two-platform OCI base using the reviewed digest and Git pins.",
		binariesMismatch:    "Rebuild all six binaries for their named targets from the admitted commit.",
		chartStamp:          "Put each chart version and image digest placeholder on exactly one full line.",
		latestInvalid:       "Mark a vMAJOR.MINOR.PATCH release as latest on GitHub, then retry.",
		storeFailure:        "Read the GitHub answer in Detail and check the token's release write access.",
		tagMissing:          "Push the release tag to GitHub, then retry publication.",
		assetUnconfirmed:    "Check the named asset on GitHub; delete it only if its upload is incomplete.",
		immutable:           "Use the existing identical release or publish a new version.",
		notVerified:         "Run clean verification for these exact release bytes before advancing latest.",
		tokenShared:         "Run chmod 600 on the token file, then retry.",
		assetsDiffer:        "Delete the assets that are not signed files from the GitHub draft, then retry.",
		tokenInside:         "Move the token file out of the release directory, then retry.",
	}[r]
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		var message interface {
			error
			Remedy() string
		}
		if !errors.As(err, &message) {
			message = failure
		}
		fmt.Fprintln(os.Stderr, message.Error())
		fmt.Fprintln(os.Stderr, message.Remedy())
		if detail, ok := message.(interface{ Detail() string }); ok {
			fmt.Fprintln(os.Stderr, "Detail:", detail.Detail())
		} else if err.Error() != message.Error() {
			fmt.Fprintln(os.Stderr, "Detail:", err)
		}
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usage
	}
	o := options{git: staticgit.Qualified}
	flags := releaseFlags(&o)
	err := flags.Parse(args[1:])
	if errors.Is(err, flag.ErrHelp) || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		flags.SetOutput(os.Stdout)
		flags.Usage()
		return nil
	}
	if err != nil || flags.NArg() != 0 {
		return usage
	}
	return runOperation(ctx, args[0], o)
}

func runOperation(ctx context.Context, operation string, o options) error {
	switch operation {
	case "base":
		return checkBase(o)
	case "package":
		return packageRelease(ctx, o)
	case "key":
		return writeKey(o.publicKey)
	case "pin":
		return releasePinCommand(o)
	case "signature":
		return verifySignature(ctx, o)
	case "verify":
		if err := verify(ctx, o); err != nil {
			return err
		}
		_, err := fmt.Fprintln(os.Stdout, "Release verified.")
		return err
	case "publish":
		return publish(ctx, o)
	case "finalize":
		return finalize(ctx, o)
	default:
		return usage
	}
}
func verify(ctx context.Context, o options) error {
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	release, err := releaseverify.Verify(ctx, root.FS(), o.version)
	if err != nil {
		return err
	}
	if err := release.CheckFiles(); err != nil {
		return err
	}
	if o.receipt != "" {
		return verifyPublished(ctx, o, release)
	}
	return nil
}
func loadVerified(ctx context.Context, o options) (releaseverify.Record, error) {
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return releaseverify.Record{}, err
	}
	defer func() { _ = root.Close() }()
	release, err := releaseverify.Verify(ctx, root.FS(), o.version)
	if err != nil {
		return releaseverify.Record{}, err
	}
	return release.Record(), nil
}

func checkBase(o options) error {
	index, err := layout.ImageIndexFromPath(o.base)
	if err != nil {
		return err
	}
	hash, err := index.Digest()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "Base index:", hash.String())
	if _, err = checkedBase(o); err != nil || o.gitOut == "" {
		return err
	}
	return exportGit(o, index)
}
func writeKey(candidate string) error {
	key, err := releaseverify.PublicKey()
	if err != nil {
		return releaseverify.Key
	}
	if candidate != "" {
		public, err := readPublicKey(candidate)
		if err != nil {
			return err
		}
		if !bytes.Equal(public, key) {
			return signingKeyMismatch
		}
	}
	_, err = os.Stdout.Write(key)
	return err
}
