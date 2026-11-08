Hand-written fixture for internal/plans: one plan that exercises most parser
rules at once. Line numbers matter to the tests; append, don't insert.

## A level-2 heading comes first

# Edge-case plan

Prose with a pipe | in it is not a table, and neither is the example below.

```markdown
| Package | Needs |
|---|---|
| **Z1 Fenced** | — |
| **Z2 Fenced** | Z1 |
```

## Work

| **Package** | What | Owner | Status | Depends on (IDs) |
|:---|:---:|---|---|--:|
| **X1 Listen to the takes** | `a | b` stays in one cell | you | | — |
| **X2** — Escaped \| pipe | Plain | | Done | X1 |
<!-- | **X9 Commented out** | | | | X1 | -->
| __X3 Underscore bold__ ✅ | | | | `X1`, **X2**; you: sign off, then tag |
| [**X4 Linked**](https://example.com) | | | Merged | X1; X2 |
| X5 No bold | | | not done | none |
| **Sam listens** | no ID here | you | | X1 |
| **X2 Duplicate** | | | | X1 |
| **X6 Unknown and loose** | | | | X1, Z9, X2 and X3 |
| **X7 Ragged** | too few cells |
| **X8 Self** | | | ✅ | X8, X1 |

## Other tables

| Thing | Needs |
|---|---|
| a | b |

| # | Decision | Decided |
|---|---|---|
| D1 | Something | **Yes** |

| Package | What |
|---|---|
| **Y1 Forgot the Needs column** | ignored, with a warning |

## Lanes

| Lane | Package | Notes |
|---|---|---|
| Listening | X1 | Single Writer: one listener |
| Tags | X2, X3, Q7 | |
| Again | X1 | |
| | X4 | no name |

| Lane | Rule |
|---|---|
| Docs | X5 owns `docs/` alone; single-writer |
