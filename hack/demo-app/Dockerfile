# A demonstration app for the debugger environment.
#
# It exists so something can actually be pushed, built and served end to end. It
# is deliberately the smallest thing that proves the whole path, so a failure
# here is the pipeline's failure rather than a toolchain's.
#
# The base image is Python's rather than nginx's for a reason that is not
# obvious: applab drops **all** capabilities on an app container
# (internal/deploy/deployer.go), and nginx's master process needs CAP_SETUID and
# CAP_SETGID to drop its workers to the nginx user — so a stock nginx image
# exits with "setgid() failed" before it ever listens. Python's http.server has
# no such requirement: it binds an unprivileged port and serves, which is all
# this needs to do.
FROM python:3.12-alpine

WORKDIR /app

COPY index.html /app/index.html

# applab sets PORT on every app container to the port the app was created with,
# and probes that port with a TCP connect. Honouring it is what makes the
# applab-configured port and the port actually bound the same number, instead of
# two numbers that have to be kept in step by hand.
ENV PORT=8080

# The shell form is deliberate: ${PORT} has to be expanded when the container
# starts, which is what makes the ENV above take effect.
CMD python3 -m http.server ${PORT} --bind 0.0.0.0

EXPOSE 8080
