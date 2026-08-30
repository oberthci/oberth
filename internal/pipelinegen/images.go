// Package pipelinegen turns what a repository already says about how it is
// built into an Oberth pipeline document.
//
// The previous generator emitted a fixed demo DAG that copied a file and
// checksummed it. It passed on every repository, which is the problem: a
// pipeline that tests nothing is indistinguishable from a pipeline that
// works, and the first honest signal arrived only when someone replaced it by
// hand. Everything here is built so that when translation cannot produce
// something that really runs the repository's build, the output says so
// instead of going green.
package pipelinegen

import "fmt"

// Runner images must be pinned by digest and must start with one of the
// administrator's allowed prefixes (golang:, debian:, node:, maven:,
// aquasec/trivy:). A tag alone is refused at admission, because a registry
// writer can move a tag between admission and node pull.
//
// The digests are resolved and pinned here rather than looked up at init time:
// `oberth init` runs in a repository checkout, often behind a proxy that has
// no route to a registry, and a generator that needs the network to produce a
// file is a generator that fails at the worst moment. Refresh with
// `crane digest <image>`; the comment on each line is the command.
const (
	// crane digest node:20-trixie-slim
	imageNode20 = "node:20-trixie-slim@sha256:abfbe12cc943141a0c9e8c0a57d710df1dadd95d35e8662cc02958b284d1f35b"
	// crane digest node:22-trixie-slim
	imageNode22 = "node:22-trixie-slim@sha256:7b8a0c89c54499bee567618f96578e1a12a800f062fbdbfd1fb6a443fa6f6284"
	// crane digest node:24-trixie-slim
	imageNode24 = "node:24-trixie-slim@sha256:ab3eebe934147fee049b5eb83c570f68c849a13c930bdfa482de99fcdfa3b3de"

	// crane digest maven:3.9-eclipse-temurin-17
	imageMaven17 = "maven:3.9-eclipse-temurin-17@sha256:a8746f15d5bb26b5b8bacb056cc76211553850f4c71d16aff845cfa004cbc197"
	// crane digest maven:3.9-eclipse-temurin-21
	imageMaven21 = "maven:3.9-eclipse-temurin-21@sha256:8f6ac126f7810bb5549c4cd122d2bf0e9cda5bdeb0838aa928f09e779fd8bef8"
	// crane digest maven:3.9-eclipse-temurin-24
	imageMaven24 = "maven:3.9-eclipse-temurin-24@sha256:a137a467ec89b5713d0be817b55bdba6b4d6ef16e3d05565a79bc08d8e775a1c"
	// crane digest maven:3.9-eclipse-temurin-25
	imageMaven25 = "maven:3.9-eclipse-temurin-25@sha256:d67198007bb4441b07d45587320f83154de80ece3608f80408ef14c6ea847753"

	// crane digest golang:1.24-trixie
	imageGo = "golang:1.24-trixie@sha256:5835f052b784aa39f2fe9070def3568605c8bc3fcd810f10402066348b61e716"
	// crane digest debian:trixie-slim
	imageDebian = "debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132"
)

// nodeImage picks the pinned image for a declared major version.
//
// An unknown version does not fall back silently: the caller records the
// substitution in the generated header, because a pipeline built on a
// different major than the repository asked for is a real difference and
// finding it in a build log is worse than reading it at the top of the file.
func nodeImage(major string) (image string, exact bool) {
	switch major {
	case "20":
		return imageNode20, true
	case "22":
		return imageNode22, true
	case "24":
		return imageNode24, true
	default:
		return imageNode22, false
	}
}

// mavenImage picks the pinned Maven image carrying a given JDK major.
func mavenImage(major string) (image string, exact bool) {
	switch major {
	case "17":
		return imageMaven17, true
	case "21":
		return imageMaven21, true
	case "24":
		return imageMaven24, true
	case "25":
		return imageMaven25, true
	default:
		return imageMaven21, false
	}
}

// substitutionNote is the line the header carries when an exact image was not
// available for the version the repository declared.
func substitutionNote(tool, wanted, used string) string {
	return fmt.Sprintf("%s %s has no pinned image here; this pipeline runs %s instead. Re-pin it before trusting a green run.", tool, wanted, used)
}
