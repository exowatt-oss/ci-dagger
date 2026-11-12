package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/exowatt-oss/ci-dagger/internal/dagger"
)

var ghRepoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)

// checkGitSSHKeys validates --ssh-key / --ssh-key-repo (python-uv).
func checkGitSSHKeys(keys []*dagger.Secret, repos []string) error {
	if len(keys) == 0 {
		if len(repos) > 0 {
			return fmt.Errorf("--ssh-key-repo without --ssh-key")
		}
		return nil
	}
	if (len(keys) > 1 || len(repos) > 0) && len(repos) != len(keys) {
		return fmt.Errorf("--ssh-key-repo must name the GitHub repo of every --ssh-key (%d keys, %d repos)", len(keys), len(repos))
	}
	for _, r := range repos {
		if !ghRepoRE.MatchString(r) {
			return fmt.Errorf("--ssh-key-repo %q: want Owner/name", r)
		}
	}
	return nil
}

// withGitSSHKeys mounts read-only deploy keys for git-over-SSH to github.com.
// One key without a repo: every github.com fetch uses it. Otherwise each key
// gets its own ssh Host alias and a git url.insteadOf rewrite (environment
// only, GIT_CONFIG_*) for its repo, so two repo-scoped deploy keys work in
// one exec. Keys are secret mounts (never a layer); the copy is normalised
// (CR stripped, trailing newline; env-sourced keys often lose it and OpenSSH
// then fails with "error in libcrypto").
func withGitSSHKeys(ctx context.Context, c *dagger.Container, keys []*dagger.Secret, repos []string) (*dagger.Container, error) {
	if len(keys) == 0 {
		return c, nil
	}
	var cfg strings.Builder
	n := 0
	for i, k := range keys {
		raw, err := k.Plaintext(ctx)
		if err != nil {
			return nil, fmt.Errorf("--ssh-key %d: %w", i+1, err)
		}
		key := strings.TrimRight(strings.ReplaceAll(raw, "\r", ""), "\n") + "\n"
		path := fmt.Sprintf("/run/secrets/git_ssh_key_%d", i)
		c = c.WithMountedSecret(path, dag.SetSecret("git-ssh-key-"+nonce(), key),
			dagger.ContainerWithMountedSecretOpts{Owner: "root", Mode: 0o400})
		host := "github.com"
		if len(repos) > 0 {
			host = fmt.Sprintf("github-ci-key-%d", i)
			to := "ssh://git@" + host + "/" + repos[i]
			for _, from := range []string{"ssh://git@github.com/" + repos[i], "git@github.com:" + repos[i]} {
				c = c.WithEnvVariable(fmt.Sprintf("GIT_CONFIG_KEY_%d", n), "url."+to+".insteadOf").
					WithEnvVariable(fmt.Sprintf("GIT_CONFIG_VALUE_%d", n), from)
				n++
			}
		}
		fmt.Fprintf(&cfg, "Host %s\n  HostName github.com\n  User git\n  IdentityFile %s\n  IdentitiesOnly yes\n", host, path)
	}
	if n > 0 {
		c = c.WithEnvVariable("GIT_CONFIG_COUNT", fmt.Sprint(n))
	}
	return c.
		WithNewFile("/root/.ssh/known_hosts", githubKnownHosts, dagger.ContainerWithNewFileOpts{Permissions: 0o644}).
		WithNewFile("/root/.ssh/config", cfg.String(), dagger.ContainerWithNewFileOpts{Permissions: 0o600}).
		WithEnvVariable("GIT_SSH_COMMAND", "ssh -o BatchMode=yes "+sshOpts), nil
}
