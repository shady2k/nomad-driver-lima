# One gateway, independent Prime coordinator and workers

Source release0.1.9; not installed by this change. All-model native config needs a
NEW image with the three frozen guest assets. Do not mutate an existing bound VM,
published v4 image, caller target config or live Prime process. Go driver0.1.8 is
unchanged and reusable. No new service, endpoint, auth method or credential exists.

The operator pins `model_catalog_file` and `model_catalog_sha256` to the reviewed
Prime-compatible definitions JSON. `models` lists every exact ID, supported levels,
capability ceilings, unsupported reasons and native selectors under ONE gateway.
Metadata compatibility is not proof of upstream availability. No model alias is
invented: the repo's exact sol ID is `gpt-6.1-sol`, not `sol`.

`request --model EXACT_ID` chooses the initial coordinator. Omission retains the
operator default. Future guest config includes all compatible definitions and
accurate per-model capabilities; no small smoke/work profile is substituted.
`model-select --session UUID --selection UUID --model EXACT_ID` changes only the
coordinator in an existing ready/live-owner ACP session using official
`acpx set model OPAQUE_VALUE -s SESSION`. It grants no launch permit. Before the
first persistent owner exists, choose the coordinator at request time instead.
Control acknowledgment must be owner-routed (resumed=false) and the exact record.
Durable intent/revision and normal owner/attempt/finalization guards apply. Lost
response remains unknown, blocks further work and is NOT automatically retried.
Repeated selection ID reads its receipt, not another native control request.

Workers are native Prime RLM, not an OpenAI subagents API. Each independent call:

```python
handle = await rlm.spawn(prompt, name="worker-name", model=EXACT_WORKER_SELECTOR,
                         thinking=SUPPORTED_LEVEL)
```

Use the worker_selector from `models`, independently of coordinator_acp_value.
Omitting model invokes upstream parent inheritance; the plugin never copies a
coordinator setting into a global worker default. Existing workers are not retargeted
by a coordinator selection. Native supported-level checks still apply.

Prime0.10.1 source: pa-daemon/src/acp/{types,config_options,wire_config}.rs;
pa-daemon/src/rlm_child_model.rs; prime-agent-runtime/src/rlm/__init__.py.
Custom pa-core/src/models/custom.rs builds custom models from PROVIDER compat;
registry.rs applies modelOverrides only to built-ins before custom replacement.
Thus different native compat sets require deterministic internal provider blocks.
All use the SAME configured HTTPS gateway, `apiKey: "LLM_API_KEY"` and stable session
header. Original model IDs/capabilities stay exact. No model-level endpoint/auth/
header override is accepted. The user-facing catalog is not split into policies.

Pinned f02d19280962ce9a6ec5b72dc8457a66ccbb4a8c contains ten compatible definitions:
glm-5.3-flash, glm-5.3-flash-auto, glm-5.3-flashx, glm-5.3,
deepseek-v4.1-flash, deepseek-v4.1-flash-auto, deepseek/deepseek-v4-flash,
gpt-6-luna, gpt-6.1-sol, gpt-6.1-terra. There are two native compat groups: GPT,
and GLM/DeepSeek. They are implementation detail, not new providers/credentials.

Image handoff: refresh a NEW immutable version using task-main.py, adapter.mjs and
launcher.mjs; preserve v4 for rollback. No dependencies need reinstalling. After
no-model native catalog/ACP readiness acceptance, mark tools.prime_model_catalog="1"
in that accepted catalog entry. Caller fails preflight for old images rather than
creating a VM that cannot parse all-model config. Activation is coordinated by the
runtime executor. Stage provider via deploy/provider-native-capabilities.py
--guest-gateway-output, not the old selected-single-definition output; model catalog
file points to the reviewed definitions output. Do not change active target configs.

The user removed mandatory plugin monetary approval: no amount question or extra
monetary cap before the requested smoke/work. Existing gateway quotas/billing and
all capability/ownership/network boundaries remain unchanged. Preparation still
uses one short synthetic functional smoke, then stops before project work. No
blind request replay; no claim that ACP has a per-request output override.
