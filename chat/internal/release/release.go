// Package release holds the constants pinned per Warden release: runtime
// versions and checksums, guest images and the public OAuth client IDs. An
// upgrade is a new binary; none of these belong in warden.json.
package release

// Revision identifies the build of every Warden binary. The release workflow
// sets it at link time with
//
//	-ldflags "-X warden/chat/internal/release.Revision=<version>"
//
// and each binary prints it with --version. Binaries of different revisions
// may run together while they share the same Protocol (OVH updates one
// container at a time); warden-chat logs a warning when a peer's revision
// differs and refuses to start when the protocol differs.
var Revision = "development"

// Protocol is the version of the private protocols between the Warden
// services taken together: the runner's worker protocol (the sandbox
// package's ProtocolVersion), the policy control protocol and the startup
// handshake. Bump it when any of them changes incompatibly. Every binary
// prints it after its revision ("protocol=N"); warden-chat checks the runner
// and the policy service against it at startup and `warden start` refuses
// to launch a set of binaries whose numbers differ.
const Protocol = 2

// Pinned runtime versions. The runner checks these. SBX itself is not
// pinned: Warden requires an sbx executable and assumes it has the features
// it uses; a missing feature fails at the call that needs it, not up front.
// SBXTestedVersion records what the acceptance runs used, for messages only.
const (
	SBXTestedVersion = "0.42.1"
	CodexVersion     = "0.154.0"
	ClaudeVersion    = "2.1.272"
)

// StockTemplate is the mountless shell SBX template new sandboxes are created
// from when no Warden guest image is installed. StockTemplateDigest is the
// digest of its multi-architecture OCI index (linux/amd64 and linux/arm64),
// which is what `sbx inspect` reports as image_digest for a sandbox the
// daemon created from that reference, on x86_64 Linux and on Apple Silicon
// alike (see docs/warden-guest-image-plan.md, "Two architectures").
const (
	StockTemplate       = "docker/sandbox-templates:shell-docker"
	StockTemplateDigest = "sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919"
)

// StockTemplatePlatformDigests are the per-platform image manifests the
// StockTemplateDigest index points at (Docker Hub, queried 2026-09-15). A copy
// of the stock template loaded from a single-platform tar with `sbx template
// load` reports its manifest digest, not the index digest, so a verifier on a
// host of that architecture must accept it beside the index.
var StockTemplatePlatformDigests = map[string]string{
	AMD64: "sha256:53b08fa716a1725f5a238b69e85f05f96956e621943d7a5c4466502321b07cfd",
	ARM64: "sha256:d353bf15d949bfb5a9de5338cc1285949a9e83d575c185958e449a19d9150190",
}

// StockTemplateDigests lists every digest a sandbox created from the stock
// template may report on a host of the given architecture. Every entry names
// the same image content: the index, or the platform manifest inside it. On
// amd64 the set is exactly the index digest, as verified on OVH; the arm64
// set also admits the arm64 manifest so a Mac install that loads the stock
// template from a tar, or a daemon that reports the platform manifest, still
// verifies. An unknown architecture gets only the index.
func StockTemplateDigests(arch string) []string {
	digests := []string{StockTemplateDigest}
	if arch != AMD64 {
		if digest, ok := StockTemplatePlatformDigests[arch]; ok {
			digests = append(digests, digest)
		}
	}
	return digests
}

// Runtime describes one downloadable agent runtime for one guest architecture.
type Runtime struct {
	URL    string
	SHA256 string
}

// Guest architectures, named as Go reports them for the host.
const (
	AMD64 = "amd64"
	ARM64 = "arm64"
)

// CodexBundles are the official Codex "package" bundles by guest architecture.
// Both checksums were computed from downloads on 2026-09-15 and match the
// release's codex-package_SHA256SUMS file and the GitHub API asset digests.
var CodexBundles = map[string]Runtime{
	AMD64: {
		URL:    "https://github.com/openai/codex/releases/download/rust-v" + CodexVersion + "/codex-package-x86_64-unknown-linux-musl.tar.gz",
		SHA256: "fc6e3e3b85f2cf7d664520ee5c66a7fe4aa12bae7d46834f47e2f165fd0d6f78",
	},
	ARM64: {
		URL:    "https://github.com/openai/codex/releases/download/rust-v" + CodexVersion + "/codex-package-aarch64-unknown-linux-musl.tar.gz",
		SHA256: "97d93e11df72d3c26772db019e6ea8bb72c246500d46b98c760839f3240355e6",
	},
}

// ClaudeExecutables are the official native Linux Claude Code releases (the
// glibc builds, which the Ubuntu guest runs). Both checksums were computed
// from downloads on 2026-09-15 and match the release's manifest.json
// (platforms.linux-x64 and platforms.linux-arm64).
var ClaudeExecutables = map[string]Runtime{
	AMD64: {
		URL:    "https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases/" + ClaudeVersion + "/linux-x64/claude",
		SHA256: "d81396a668eb76fbddb49a2a5841f1b5d7af96b4c1f6500ced92f2c988f5bcd4",
	},
	ARM64: {
		URL:    "https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases/" + ClaudeVersion + "/linux-arm64/claude",
		SHA256: "214a90efdd16ee0ea81132ffecced588dba394d178cc494f285ba04b5288c8de",
	},
}

// GuestImages are the published Warden guest images by architecture. The
// workflow pushes one multi-platform index per commit; Digest is that
// platform's image manifest digest (printed per platform in the workflow
// summary), never the index digest, because install.sh pulls and loads one
// platform's manifest and that is what `sbx inspect` then reports. Empty
// until the guest image workflow publishes them; the installer falls back to
// the stock template and the runner copies runtimes in.
var GuestImages = map[string]struct{ Ref, Digest string }{
	AMD64: {Ref: "ghcr.io/punished-monaddle/warden-guest", Digest: ""},
	ARM64: {Ref: "ghcr.io/punished-monaddle/warden-guest", Digest: ""},
}

// Public OAuth client identifiers shipped with Warden for local mode. Both
// are empty until the registrations are made; logins fail with a clear
// message rather than a placeholder ID.
const (
	// GitHubOAuthClientID is the Warden GitHub OAuth App (device flow).
	GitHubOAuthClientID = "Ov23lijlKrGFnq0DclLn" // OAuth App "Warden" (device flow), registered 2026-09-16
	// GoogleDocsClientID and GoogleDocsClientSecret are the Desktop-type
	// Google client for the Docs connection. Google treats Desktop client
	// secrets as non-confidential; Warden still redacts it and never gives it
	// to guests.
	GoogleDocsClientID     = "730944705291-vne7sjg9uuheu4gotbk3ems1ukm9u8jd.apps.googleusercontent.com" // Desktop client "Warden Docs (local)", project boreal-doodad-508223-c8, 2026-09-16
	GoogleDocsClientSecret = "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"                                      // Desktop-type secret: non-confidential by Google's definition; never given to guests
)
