# hector
Throw-away Discord bot, vibe-coded over-night by yours truly on a freakin’ iPad.

## Project structure

- `cmd/hector` – application entry point
- `internal/config` – environment config loading
- `internal/bot` – Discord command handling, context lookups, and prompt assembly
- `internal/gemini` – Gemini API client
- `.env.example` – template for required environment variables

## Setup

1. Copy `.env.example` to `.env`
2. Fill in your Discord bot token and Gemini API key
3. In the Discord Developer Portal, enable the **Message Content Intent** for the bot
4. Adjust the optional config values if needed
5. Run:

```bash
go run ./cmd/hector
```

## Configuration

The app reads the following values from `.env` or the shell environment:

- `DISCORD_TOKEN` – Discord bot token
- `GEMINI_API_KEY` – Gemini API key
- `GEMINI_MODEL` – default `gemini-3.5-flash-lite`; requests use Gemini’s Interactions API
- `BOT_NAME` – default `Hector`; the name used for identity and name-based triggering
- `BOT_PREFIX` – default `h`
- `MAX_CONTEXT_MESSAGES` – default `8`; used for `h ctx <N> ...`
- `SYSTEM_PROMPT` – system prompt sent to Gemini
- `HELP_TEXT` – text shown when the user sends a blank prompt
- `MAX_RESPONSE_CHARS` – max output length, default `140`
- `MAX_OUTPUT_TOKENS` – Gemini output-token ceiling, default `512`; optional thinking is disabled to preserve room for the visible response
- `CODE_SEARCH_MAX_BYTES` – maximum CodeSearch output size, default `50000`
- `CODE_SEARCH_TIMEOUT_SECONDS` – CodeSearch timeout, default `5`

## Usage

Use either the configured prefix or a mention:

```text
h hello
@hector hello
```

The mention form can appear anywhere in the message, including `@Hector hello`
or `Can you ask @Hector about this?`. Discord's actual user mention format
(`<@USER_ID>`) is also accepted at the start of a message.

For a context-aware lookup, use the explicit context subcommand. It looks back through the last N messages in the active channel before answering:

```text
h ctx 5 summarize what we discussed
@hector ctx 3 what did we decide earlier?
```

If you do not use `ctx`, the bot responds using the current message only, with no automatic background history lookup. This keeps normal responses fast while still allowing deeper lookups when needed.

The bot also trims output to the configured `MAX_RESPONSE_CHARS` value so it does not burn through token budget.

Discord user mentions must use the platform's ID-based format:

```text
<@USER_ID>
```

The bot supplies user IDs to Gemini and instructs it not to emit plain `@username` mentions.

For source-code lookup, pass the literal text to search for:

```text
h code -Fi replyContext
```

CodeSearch runs a fixed-string, case-insensitive grep with three lines of context before and after each match. It does not call Gemini.

To see the commit the running process was built from, use:

```text
h version
```

The response includes the short Git revision, commit header, commit description, build timestamp, Go version, and whether Go detected modified source at build time.
