# Prime 0.10.1 native model contract

New versioned guest source accepts optional `prime_vault.provider.model_definition`.
The operator selects one reviewed declarative model from its pinned provider artifact;
no provider JavaScript executes. Existing configs without the field remain compatible.
This changes no installed guest or caller until coordinated release distribution.

Required model fields: `id` (exact provider.model), `api` (`openai-completions`),
`input` (`["text"]`), `reasoning` (boolean), `contextWindow`, `maxTokens`.
Optional: `name`, `thinkingLevelMap`, `compat`. New config generation copies the
selected pinned repository's contextWindow/maxTokens exactly into both the model
definition and legacy provider context_window/max_tokens fields. One provider/model
profile serves preparation and work; a smoke must not install a smaller/fake model.
The existing guest runtime still accepts legacy operator limits for compatibility;
new generation no longer uses that override. No guest/image rebuild is needed.

Prime 0.10.1 ACP PromptParams contains only sessionId/prompt; session config options
are model/thought_level. There is no supported per-turn output-token override.
Short synthetic input, requesting a short answer, deadline/cancel and no automatic
retry are operational constraints, NOT a hard monetary cap or guaranteed one LLM
request. Autonomous usage limits are a different mode and post-response accounting,
not a substitute. The plugin does not impose a monetary limit or ask for a dollar
amount before the user-requested smoke/work. Existing gateway billing and quotas
are unchanged. Preserve capability ceilings and unknown-outcome/no-retry safety.
Do not expose fake smoke/work model profiles or invent unsupported request fields.

Prime v0.10.1 source evidence: `crates/pa-core/src/models/custom.rs`,
`ModelDefinition` and `load_custom_models` custom-model construction; compat types
in `crates/pa-models/src/compat.rs`. Native `thinkingLevelMap` levels are off,
minimal, low, medium, high, xhigh, max (null means unsupported). Prime does NOT
consume the other host's `thinking` effort profile. It must be omitted by the
explicit Prime projection, not sent expecting effect. For newly defined custom
models Prime constructs compat from PROVIDER config, so this single-model profile
places the validated selected compat there; model-level compat alone is ineffective.

Allowed compat booleans: zaiToolStream, sendSessionAffinityHeaders, supportsStore,
supportsDeveloperRole, supportsReasoningEffort, supportsUsageInStreaming,
requiresToolResultName, requiresAssistantAfterToolResult, requiresThinkingAsText,
requiresReasoningContentOnAssistantMessages, supportsStrictMode,
supportsLongCacheRetention. Allowed enums: maxTokensField (max_tokens or
max_completion_tokens), thinkingFormat (openai/openrouter/deepseek/zai/qwen/
qwen-chat-template), cacheControlFormat (anthropic).

Common multi-host declarations supportsReasoningParams, clampOutputToModelMax,
replayReasoningContent, requiresReasoningContentForAllAssistantTurns,
requiresReasoningContentForToolCalls are NOT supported Prime 0.10.1 compat fields.
The deployment adapter must explicitly project these out and report unsupported
fields. Routing objects are intentionally unsupported in this narrow gateway
contract. Any unknown field fails validation. Endpoint, API-key lookup, session
header and OpenBao configuration cannot be overridden by model_definition; no
headers/auth/code/cost overrides are accepted. Availability, pricing and actual
provider behavior remain unproven by metadata or offline validation.
