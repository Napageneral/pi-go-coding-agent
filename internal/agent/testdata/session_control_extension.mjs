export default function extension(pi) {
	pi.registerCommand("newsession", {
		description: "Create a new session",
		handler: async (_args, ctx) => {
			await ctx.newSession();
			return "new-session-ok";
		},
	});

	pi.registerCommand("switchsession", {
		description: "Switch to session path",
		handler: async (args, ctx) => {
			const path = String(args || "").trim();
			await ctx.switchSession(path);
			return "switch-session-ok";
		},
	});

	pi.registerCommand("forkat", {
		description: "Fork at entry id",
		handler: async (args, ctx) => {
			const entryId = String(args || "").trim();
			await ctx.fork(entryId);
			return "fork-ok";
		},
	});

	pi.registerCommand("navigate", {
		description: "Navigate tree to entry id",
		handler: async (args, ctx) => {
			const entryId = String(args || "").trim();
			await ctx.navigateTree(entryId);
			return "navigate-ok";
		},
	});

	pi.registerCommand("reloadcmd", {
		description: "Trigger reload",
		handler: async (_args, ctx) => {
			await ctx.reload();
			return "reload-ok";
		},
	});

	pi.registerCommand("waitcmd", {
		description: "Wait for idle",
		handler: async (_args, ctx) => {
			await ctx.waitForIdle();
			return "wait-ok";
		},
	});
}
