function hasModelRegistryShape(modelRegistry) {
	const required = [
		"getAvailable",
		"find",
		"getApiKey",
		"getApiKeyForProvider",
		"getApiKeyByProvider",
		"isUsingOAuth",
		"getError",
	];
	return required.every((name) => typeof modelRegistry?.[name] === "function");
}

function hasContextShape(ctx) {
	const required = [
		"sessionManager",
		"modelRegistry",
		"model",
		"isIdle",
		"hasPendingMessages",
		"getSystemPrompt",
	];
	return required.every((name) => Object.prototype.hasOwnProperty.call(ctx || {}, name));
}

function containsModel(models, provider, modelId) {
	if (!Array.isArray(models)) return false;
	return models.some((model) => {
		if (!model || typeof model !== "object") return false;
		const p = typeof model.provider === "string" ? model.provider : "";
		const id = typeof model.id === "string" ? model.id : "";
		return p === provider && id === modelId;
	});
}

export default function extension(pi) {
	pi.registerCommand("ctxdiag", {
		description: "Dump extension context/model registry diagnostics",
		handler: async (_args, ctx) => {
			const model = ctx.model && typeof ctx.model === "object" ? ctx.model : {};
			const provider = typeof model.provider === "string" ? model.provider : "";
			const modelId = typeof model.id === "string" ? model.id : "";

			const available =
				typeof ctx.modelRegistry?.getAvailable === "function" ? await ctx.modelRegistry.getAvailable() : [];
			const foundCurrent =
				provider &&
				modelId &&
				typeof ctx.modelRegistry?.find === "function" &&
				ctx.modelRegistry.find(provider, modelId);

			const apiKeyFromModel =
				modelId && typeof ctx.modelRegistry?.getApiKey === "function"
					? await ctx.modelRegistry.getApiKey(model)
					: undefined;
			const apiKeyFromProvider =
				provider && typeof ctx.modelRegistry?.getApiKeyForProvider === "function"
					? await ctx.modelRegistry.getApiKeyForProvider(provider)
					: undefined;
			const apiKeyByProvider =
				provider && typeof ctx.modelRegistry?.getApiKeyByProvider === "function"
					? await ctx.modelRegistry.getApiKeyByProvider(provider)
					: undefined;
			const usingOAuth =
				provider && typeof ctx.modelRegistry?.isUsingOAuth === "function"
					? Boolean(ctx.modelRegistry.isUsingOAuth(model))
					: false;

			return JSON.stringify({
				hasContextShape: hasContextShape(ctx),
				hasModelRegistryShape: hasModelRegistryShape(ctx.modelRegistry),
				modelProvider: provider,
				modelId,
				foundCurrentModel: Boolean(foundCurrent),
				availableCount: Array.isArray(available) ? available.length : 0,
				availableHasCurrent: containsModel(available, provider, modelId),
				systemPrompt: ctx.getSystemPrompt(),
				isIdle: ctx.isIdle(),
				hasPendingMessages: ctx.hasPendingMessages(),
				contextUsageVisible: typeof ctx.getContextUsage === "function",
				thinkingLevel: pi.getThinkingLevel(),
				apiKeyForModelPresent: typeof apiKeyFromModel === "string" && apiKeyFromModel.length > 0,
				apiKeyForProviderPresent: typeof apiKeyFromProvider === "string" && apiKeyFromProvider.length > 0,
				apiKeyByProviderPresent: typeof apiKeyByProvider === "string" && apiKeyByProvider.length > 0,
				usingOAuth,
			});
		},
	});

	pi.registerCommand("ctxusage", {
		description: "Get context usage snapshot",
		handler: async (_args, ctx) => {
			const usage =
				typeof ctx.getContextUsage === "function" ? ctx.getContextUsage() : undefined;
			const hasUsage = usage && typeof usage === "object";
			return JSON.stringify({
				hasUsage: Boolean(hasUsage),
				tokens: hasUsage ? usage.tokens : null,
				contextWindow: hasUsage ? usage.contextWindow : 0,
				percent: hasUsage ? usage.percent : null,
			});
		},
	});

	pi.registerCommand("ctxsetthinking", {
		description: "Set thinking level and return current level",
		handler: async (args) => {
			const next = String(args || "").trim() || "high";
			pi.setThinkingLevel(next);
			return `ctxsetthinking:${pi.getThinkingLevel()}`;
		},
	});

	pi.registerCommand("ctxcompact", {
		description: "Trigger context compaction",
		handler: async (args, ctx) => {
			const customInstructions = String(args || "").trim();
			ctx.compact({
				customInstructions: customInstructions || undefined,
			});
			return "ctxcompact-ok";
		},
	});
}
