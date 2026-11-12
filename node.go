package main

// sshOpts bounds git-over-SSH operations performed by npm dependencies.
const sshOpts = "-o StrictHostKeyChecking=yes -o ConnectTimeout=20 -o ServerAliveInterval=15 -o ServerAliveCountMax=4"

// GitHub's published SSH host keys (https://api.github.com/meta), pinned so
// SSH-based package dependencies do not use trust-on-first-use.
const githubKnownHosts = `github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl

github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=

github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=`

// npmCiBounded retries from a clean node_modules and bounds hangs in npm or
// git-based dependencies. A configured proxy is bypassed on the final attempt.
const npmCiBounded = `attempt=1
while :; do
  if [ "$attempt" -ge 3 ] && [ -n "${npm_config_registry:-}" ]; then
    echo "npm ci attempt $attempt: bypassing the configured registry proxy" >&2
    unset npm_config_registry
  fi
  timeout -k 15 "${NPM_CI_TIMEOUT:-300}" npm ci --no-audit --no-fund && break
  rc=$?
  if [ "$attempt" -ge 3 ]; then echo "npm ci failed (rc=$rc) after $attempt attempts" >&2; exit "$rc"; fi
  echo "npm ci attempt $attempt failed (rc=$rc; 124 = timed out); retrying" >&2
  attempt=$((attempt + 1))
  rm -rf node_modules
done`

// gitBounded is used by npm for git-based package dependencies.
const gitBounded = "#!/bin/sh\nexec timeout -k 5 \"${GIT_OP_TIMEOUT:-90}\" git \"$@\"\n"

// nodeAptPackages are installed in Node toolchain images for package builds
// that use Git over SSH.
const nodeAptPackages = "git openssh-client ca-certificates"
