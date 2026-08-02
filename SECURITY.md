# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately rather than opening a
public issue. Email **comment-dad-dating@duck.com** with:

- A description of the issue and its impact.
- Steps to reproduce, or a proof of concept if available.
- Any relevant logs, config, or environment details.

We aim to acknowledge reports within 5 business days. Please do not disclose
the issue publicly until a fix has shipped.

## Token compromise runbook

If the Discord bot token is ever leaked (committed, logged, pasted):

1. Revoke it immediately in the Discord Developer Portal (Bot -> Reset Token).
2. Update the secret in your deployment's secret manager (Docker secret /
   Vault / platform equivalent) with the new token.
3. Redeploy. The bot re-authenticates on next start with no other changes
   needed, since token rotation does not require a code change.

## Scope

This policy covers the `merlin` bot codebase and its CI/CD pipeline. It does
not cover the Discord platform itself or third-party dependencies (report
those upstream).
