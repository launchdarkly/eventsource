# Dockerfile for running the contract test service. At this point we have
# already compiled the code, so we only need to copy the executable.

FROM ubuntu:20.04

COPY contract-tests .

ENTRYPOINT [ "./contract-tests" ]
