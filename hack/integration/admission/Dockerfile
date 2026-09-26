# The extended integration suite's admission stand-in. The harness builds the
# static binary with the module's own toolchain and loads the image into kind;
# it is never pushed or pulled. The base is the operator's pinned distroless.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY admission /admission
USER 65532:65532
ENTRYPOINT ["/admission"]
