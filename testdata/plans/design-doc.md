# Booking cache: design

Hand-written fixture for internal/plans: a design doc with tables, bold
IDs and the word "needs" in its prose, but no dependency column, so it is
not a dispatch plan.

The booking page needs a cache in front of the schedule API.

## Options

| Option | What | Cost | Risk |
|---|---|---|---|
| **A1 Cache in the app** | An in-memory cache per instance. | Low | Stale across instances |
| **A2 Shared cache** | One shared cache for every instance. | Medium | One more service |
| **A3 No cache** | Call the API every time. | None | Rate limits |

## Components

| Name | Owner | Status |
|---|---|---|
| **C1 Fetcher** | you | done |
| **C2 Invalidator** | Sam | in progress |

## Decisions

| # | Decision | Decided |
|---|---|---|
| D1 | Which option | **A2**, because the API rate-limits per key. |
| D2 | TTL | 60 seconds. |

## Example of a plan table (fenced, so not a table here)

```markdown
| Package | What | Needs |
|---|---|---|
| **P1 Fetcher** | … | — |
| **P2 Invalidator** | … | P1 |
```
