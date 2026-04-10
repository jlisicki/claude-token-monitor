# claude-token-monitor

Real-time token usage and cost tracker for [Claude Code](https://docs.anthropic.com/en/docs/claude-code).

Reads Claude Code's JSONL session files and displays per-request token breakdowns with estimated costs.

## Install

```bash
go install github.com/jlisicki/claude-token-monitor@latest
```

Or build from source:

```bash
git clone https://github.com/jlisicki/claude-token-monitor.git
cd claude-token-monitor
go build
```

## Usage

### Tail mode (streaming)

```bash
token-monitor --tail
```

Streams token usage in real-time as Claude Code makes API calls:

```
──────────────── session: 45k tokens  $0.12 ────────────────
 TIME     PROJECT      MODEL   AGT TYPE      INPUT   OUTPUT    THINK   C.READ          C.WRITE C.HIT      TOTAL
 14:23:05 my-project   sonnet  main tool_use     3      847        0     28k         0@5m   99%       29k
                                               $0.00    $0.01    $0.00    $0.00           $0.00          $0.01
 14:23:12 my-project   sonnet  sub  text         5      234        0     12k      3k@5m   99%       15k
                                               $0.00    $0.00    $0.00    $0.00           $0.01          $0.01
```

Each API call shows two rows: token counts and corresponding costs.

### TUI dashboard

```bash
token-monitor
```

Interactive dashboard with aggregate token usage, per-model breakdown, and recent activity windows.

### One-shot summary

```bash
token-monitor --summary
```

Prints totals and exits.

## Flags

| Flag | Description |
|------|-------------|
| `--tail` | Stream token usage lines in real-time |
| `--summary` | Print summary and exit |
| `--history` | Include historical totals on startup (use with `--tail`) |
| `--verbose` | Show user/system message lines (use with `--tail`) |
| `--debug` | Print raw JSONL for each record (use with `--tail`) |
| `--path <dir>` | Override Claude Code projects directory (default: `~/.claude/projects`) |

## Features

- **Per-request token breakdown**: input, output, thinking, cache read, cache write
- **Cost estimation**: based on current Anthropic pricing for Opus, Sonnet, and Haiku
- **Cache TTL tracking**: shows 5-minute vs 1-hour cache write breakdown with color coding
- **Agent detection**: distinguishes main agent from subagent API calls
- **Expensive line highlighting**: flags requests that cost 3x above session average with explanation
- **Compact hints**: detects session profile (exploring, coding, reasoning, delegating) and suggests `/compact` when context size causes diminishing returns
- **Session tracking**: running totals for the current monitoring session

## How it works

Claude Code stores per-message data in JSONL files at `~/.claude/projects/<project>/<session>.jsonl`. Each assistant response includes token usage in the `message.usage` field. token-monitor watches these files with fsnotify and parses new lines as they appear.

## Cost calculation

Pricing per million tokens:

| Model | Input | Output | Cache Write | Cache Read |
|-------|------:|-------:|------------:|-----------:|
| Opus | $15.00 | $75.00 | $18.75 | $1.50 |
| Sonnet | $3.00 | $15.00 | $3.75 | $0.30 |
| Haiku | $0.80 | $4.00 | $1.00 | $0.08 |

## License

MIT
