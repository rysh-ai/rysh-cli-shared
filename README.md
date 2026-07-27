# rysh-cli-shared

Shared Go module for [Rysh](https://github.com/rysh-ai/rysh-cli-parent), the
agentic terminal multiplexer.

```sh
go get github.com/rysh-ai/rysh-cli-shared
```

| Package | What |
| --- | --- |
| `agentic` | the agentic loop — orchestration, grounding, tool dispatch |
| `provider` | LLM provider adapters and streaming |
| `msg` | message and channel types shared across the wire |
| `secretnat` | secret detection and redaction (SecretNAT): outbound values are swapped for stable placeholder tokens and restored on the way back |
| `tools` | the built-in tool implementations |
| `bridge` | transport bridge types |

This is a library, not an application. It is consumed by
[`rysh-cli-code`](https://github.com/rysh-ai/rysh-cli-code); the packages are
useful on their own but the API is versioned to serve that consumer first.

To work on it against a live CLI checkout, clone
[`rysh-cli-parent`](https://github.com/rysh-ai/rysh-cli-parent) — its `go.work`
resolves this module to your local tree, so edits are picked up without a release.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
