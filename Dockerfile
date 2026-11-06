# CI supplies the static Linux binary; coverage and lint gate its publication.
# No shell, compiler, private dependency credentials or SQL runtime is shipped.
FROM gcr.io/distroless/static-debian13@sha256:d03c145fad326bff275ec4106475ed8310426caf709decd9f8cbc494af59bc18
WORKDIR /app
COPY --chown=65532:65532 bin/querydock-go /app/querydock-go
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/querydock-go"]
