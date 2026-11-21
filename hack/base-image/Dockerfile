ARG BUILDER_IMAGE=scratch
FROM ${BUILDER_IMAGE} AS build
ARG PACKAGE_FILES
WORKDIR /git
COPY packages/ /inputs/packages/
COPY build.env /inputs/build.env
RUN set -f; /sbin/apk --repositories-file /dev/null add --no-network $PACKAGE_FILES
COPY git.tar.xz git-no-shell.patch compile.sh /inputs/
RUN /bin/sh /inputs/compile.sh
FROM scratch AS artifacts
COPY --from=build /out/ /
