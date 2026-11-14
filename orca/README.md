# Orca CLI inputs and model roles

Start from a goal, optionally with a free-text plan and an explicit repository:

```sh
orca run --goal "Fix the flaky cache test" --plan plan.md --repo owner/project
```

`--goal` generates a run ID when `--run` is omitted and invokes the planner before
work starts. Use `--run` to choose an ID. `--objective` remains a metadata-only
input for existing runs. `--plan` accepts a local file, a full Nebula post ID, or
`post:<id>` (including a unique short ID). `--plan-ref` remains accepted and now
loads its reference as well. A missing or empty plan is an error. Plain-language
recipe compilation errors also stop the run; compiled JSON recipes remain
supported through `--recipe-file`.

Each role selects a provider and model independently. JSON config uses
`models.roles`; flags override config fields:

```json
{
  "models": {
    "roles": {
      "planner": {"provider": "openai", "model": "gpt-6-astra", "effort": "high"},
      "critic": {"provider": "anthropic", "model": "claude-sonnet-5-5"},
      "dispatcher": {"provider": "openai", "model": "gpt-6.1-sol"},
      "concierge": {"provider": "anthropic", "model": "claude-haiku-5-5"},
      "router": {"provider": "openai", "model": "gpt-6.1-sol"}
    }
  }
}
```

Use `--<role>-provider`, `--<role>-model`, and `--<role>-effort`. The legacy model
strings under `models` and `--model-provider` remain accepted. Defaults select
OpenAI gpt-6.1-sol for dispatcher, concierge, and router, and gpt-6-astra at high
effort for planner. The critic defaults to Anthropic Sonnet, or OpenAI when the
planner uses Anthropic. Selecting the same provider for planner and critic is an
error. Provider clients use their respective gateway environment variables;
`--llm-gateway-url` overrides the global provider's endpoint (OpenAI by default).

The planner judges whether the approach is open. An open approach receives one
free-text critique about assumptions and the cheapest useful test, followed by
one planner revision. Only the final plan updates the backlog. A failed critic
or revision stops planning. `--record` captures requests with resolved role,
provider, model, and effort for deterministic replay through `--replay`.
