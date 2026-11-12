# llm-agent

simplistic automation to talk to local LLM agents

The experimental `ds4` provider (alias `ds4-agent`) runs the native DS4 coding
agent with `--non-interactive`, sends the prompt on stdin, and captures plain text
output. It finds `ds4-agent` on `PATH`, falling back to `../ds4/ds4-agent` relative
to the calling process. It launches from the executable's directory to load the
default `ds4flash.gguf` and Metal sources, then uses `--chdir` for the task's
workspace. Symlinks to the executable are resolved first.

```go
agent := &llmagent.Agent{Provider: "ds4"}
result, err := agent.Run(ctx, "Describe this project", workdir, llmagent.RunOptions{})
```

Use `ds4:/absolute/path/model.gguf` to select a GGUF (relative model paths resolve
from the executable's directory). Effort suffixes support `@high`, `@max`, `@none`,
or a V4.1 numeric level such as `@75`. Pass runtime flags such as `--ctx`,
`--cpu`, or `--ssd-streaming` through `Agent.ExtraArgs`. Each invocation starts
a fresh session; headless session resumption is unavailable. DS4 gets the local
provider probe timeout and cooldown. `llm-diag` also discovers the sibling build.
