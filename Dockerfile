# CI supplies the static Linux binary; coverage and lint gate its publication.
# No shell, compiler, private dependency credentials or SQL runtime is shipped.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --chown=65532:65532 bin/querydock-go /app/querydock-go
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/querydock-go"]
