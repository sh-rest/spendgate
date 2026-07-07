FROM gcr.io/distroless/static-debian12
COPY spendgate /spendgate
ENTRYPOINT ["/spendgate"]
