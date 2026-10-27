# CI supplies the static Linux binary; coverage and lint gate its publication.
# No shell, compiler, private dependency credentials or SQL runtime is shipped.
FROM gcr.io/distroless/static-debian13@sha256:433bb346cd0b6b8346462f879adaa4ac0fbb463a72901b46736ede4aeca3dadc
WORKDIR /app
COPY --chown=65532:65532 bin/querydock-go /app/querydock-go
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/querydock-go"]
