package argoworkflow

// NonrootStaticProfile is the single built-in controller configuration whose
// container/script generation is covered by the pinned Argo 4.0.8 fixtures.
// Selecting its name is not execution proof: the server verifies live identity,
// immutable configuration and freshness before each selected submission.
const NonrootStaticProfile = "nonroot-static-v1"

// NonrootProfileConfig is the canonical safe config rendered by the official
// argo-workflows 1.0.24 chart defaults with the server disabled. No archive,
// default-template, plugin, main/executor or credential configuration is allowed.
const NonrootProfileConfig = `{"failedPodRestart":{"enabled":false,"maxRestarts":3},"nodeEvents":{"enabled":true},"workflowEvents":{"enabled":true}}`

// NonrootProfileConfigSHA256 pins the complete canonical configuration.
const NonrootProfileConfigSHA256 = "5f121f7f1f5ef468eb2208b1cf7e911746c8cb6173c1d24e7374ad951af92d59"

// NonrootProfileConfigMap uses 192 bits of the content digest within a DNS label;
// verification additionally compares the complete digest and exact data bytes.
const NonrootProfileConfigMap = "oberth-argo-nr-5f121f7f1f5ef468eb2208b1cf7e911746c8cb6173c1d24e"

// NonrootProfileVolume and NonrootProfileMount belong only to the trusted
// controller. The required public config key gates startup before Argo's
// synchronous API config load; neither is mounted in a workflow Pod.
const NonrootProfileVolume = "oberth-controller-profile"
const NonrootProfileMount = "/var/run/oberth-controller-profile"

// NonrootControllerImage pins the official multi-platform controller index.
const NonrootControllerImage = "quay.io/argoproj/workflow-controller:v4.0.8@sha256:7a156419f80285859fc8f859927b6cc249f0d128e161e080dc17fca8fcbbceb6"

// NonrootExecutorImage pins the official multi-platform executor index.
const NonrootExecutorImage = "quay.io/argoproj/argoexec:v4.0.8@sha256:86a965d7eea176959351156e24f3a2fa8d8e342e477ef425009af95a12e3fe87"

// NonrootControllerAMD64 is the authenticated amd64 member of the pinned index.
// Initial runtime verification accepts that member or the exact index imageID;
// no other native-architecture execution is claimed by the current proof.
const NonrootControllerAMD64 = "quay.io/argoproj/workflow-controller@sha256:bba570982ca563738f327685562a04cdce771f18993f37e17b751a520d50c20e"
