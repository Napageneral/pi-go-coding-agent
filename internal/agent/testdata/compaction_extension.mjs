const state = {
	cancelNextCompact: false,
	customNextCompact: false,
	beforeCompact: [],
	compactEvents: [],
};

export default function extension(pi) {
	pi.on("session_before_compact", (event) => {
		const prep = event && typeof event.preparation === "object" ? event.preparation : {};
		state.beforeCompact.push({
			firstKeptEntryId: typeof prep.firstKeptEntryId === "string" ? prep.firstKeptEntryId : "",
			tokensBefore: Number.isFinite(prep.tokensBefore) ? prep.tokensBefore : 0,
			customInstructions:
				typeof event.customInstructions === "string" ? event.customInstructions : "",
		});
		if (state.cancelNextCompact) {
			state.cancelNextCompact = false;
			return { cancel: true };
		}
		if (state.customNextCompact) {
			state.customNextCompact = false;
			return {
				compaction: {
					summary: "extension-compaction-summary",
					firstKeptEntryId:
						typeof prep.firstKeptEntryId === "string" ? prep.firstKeptEntryId : "",
					tokensBefore: Number.isFinite(prep.tokensBefore) ? prep.tokensBefore : 0,
					details: { source: "compaction_extension" },
				},
			};
		}
		return undefined;
	});

	pi.on("session_compact", (event) => {
		state.compactEvents.push({
			fromExtension: Boolean(event.fromExtension),
			summary: event.compactionEntry?.summary || "",
		});
	});

	pi.registerCommand("armcancelcompact", {
		description: "Cancel next compaction",
		handler: async () => {
			state.cancelNextCompact = true;
			return "cancel-compact-armed";
		},
	});

	pi.registerCommand("armcustomcompact", {
		description: "Use custom compaction on next compact call",
		handler: async () => {
			state.customNextCompact = true;
			return "custom-compact-armed";
		},
	});

	pi.registerCommand("runcompact", {
		description: "Trigger ctx.compact from command",
		handler: async (args, ctx) => {
			const customInstructions = String(args || "").trim();
			ctx.compact({
				customInstructions: customInstructions || undefined,
			});
			return "runcompact-ok";
		},
	});

	pi.registerCommand("compactevents", {
		description: "Dump compact hook/event diagnostics",
		handler: async () => {
			return JSON.stringify({
				beforeCompact: state.beforeCompact,
				compactEvents: state.compactEvents,
			});
		},
	});

	pi.registerCommand("compactmirror", {
		description: "Dump compaction entries visible to ctx.sessionManager",
		handler: async (_args, ctx) => {
			const entries = Array.isArray(ctx?.sessionManager?.getEntries?.())
				? ctx.sessionManager.getEntries()
				: [];
			const compactions = entries.filter((entry) => entry && entry.type === "compaction");
			const latest = compactions.length > 0 ? compactions[compactions.length - 1] : null;
			return JSON.stringify({
				count: compactions.length,
				latestSummary: latest?.summary || "",
			});
		},
	});
}
