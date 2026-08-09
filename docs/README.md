# u-l10n documentation

Start with the [repository README](../README.md) for setup and the phase history, and [CLAUDE.md](../CLAUDE.md) for the five things that will bite you. Then:

| Doc | Read it when you need |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | The machinery — one sequence diagram per flow: portal write, export, asset upload, the merge transaction, import |
| [USER_FLOWS.md](USER_FLOWS.md) | The personas — translator, reviewer/approver, mobile app, operator, CI |
| [API.md](API.md) | Every endpoint: auth, roles, parameters, status codes, the error-code table |
| [DATA_MODEL.md](DATA_MODEL.md) | The schema — ER diagram, the invariants it encodes, indexes, migration conventions |
| [OPERATIONS.md](OPERATIONS.md) | Running it — CLI commands, every env var, migrations, probes, the incident playbook |
| [TESTING.md](TESTING.md) | The gates (R1/R2 and their blind spot), corpus pinning, the mutation-check doctrine |

Two rules keep these docs honest: everything in them is derived from the code, not the design docs (several design-doc figures were disproved by measurement — see TESTING.md), and when a doc and CLAUDE.md disagree about the stack, CLAUDE.md is right.
