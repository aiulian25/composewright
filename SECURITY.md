# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**. Please do not open a public issue, pull request or discussion for a security problem.

Include what you found, how to reproduce it, the version (`composewright version`), and the impact as you understand it. A minimal repository or plan that triggers the problem helps most.

## What is in scope

- A plan, README, repository or model answer that gets past the policy gate: privileges, host paths, host networking, devices, the Docker socket, `build:`, an image outside the allowlist, a granted need without `--allow-need`.
- Anything that runs before explicit approval, or a sandbox run that escapes its override (restart policy, limits, loopback ports, networks, mounts).
- A secret, the master key or an API key reaching a log, a prompt, stored output or an export it should not be in.
- An outbound connection to a host outside the allowlist, or an extracted archive writing outside its folder.
- Weakened storage encryption, key handling or file permissions.

## What is not in scope

- Behaviour of an image you trusted with `--allow-registry`, or of a need you granted with `--allow-need`.
- The Docker socket being root-equivalent on a host where you run a rootful daemon; `composewright doctor` warns about it and recommends the rootless tier.
- Vulnerabilities in the applications composewright deploys.

## Supported versions

Security fixes are made on the latest release.
