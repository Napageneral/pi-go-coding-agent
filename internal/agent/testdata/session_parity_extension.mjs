const state = {
	cancelNextSwitch: false,
	cancelNextFork: false,
	cancelNextTree: false,
	beforeSwitch: [],
	switchEvents: [],
	beforeFork: [],
	forkEvents: [],
	beforeTree: [],
	treeEvents: [],
};

function normalizeTreePreparation(prep) {
	const p = prep && typeof prep === "object" ? prep : {};
	return {
		targetId: typeof p.targetId === "string" ? p.targetId : "",
		oldLeafId: typeof p.oldLeafId === "string" ? p.oldLeafId : "",
		userWantsSummary: Boolean(p.userWantsSummary),
		customInstructions: typeof p.customInstructions === "string" ? p.customInstructions : "",
		replaceInstructions: Boolean(p.replaceInstructions),
		label: typeof p.label === "string" ? p.label : "",
	};
}

export default function extension(pi) {
	pi.on("session_before_switch", (event) => {
		state.beforeSwitch.push({
			reason: typeof event.reason === "string" ? event.reason : "",
			targetSessionFile: typeof event.targetSessionFile === "string" ? event.targetSessionFile : "",
		});
		if (state.cancelNextSwitch) {
			state.cancelNextSwitch = false;
			return { cancel: true };
		}
		return undefined;
	});

	pi.on("session_switch", (event) => {
		state.switchEvents.push({
			reason: typeof event.reason === "string" ? event.reason : "",
			previousSessionFile: typeof event.previousSessionFile === "string" ? event.previousSessionFile : "",
		});
	});

	pi.on("session_before_fork", (event) => {
		state.beforeFork.push({
			entryId: typeof event.entryId === "string" ? event.entryId : "",
		});
		if (state.cancelNextFork) {
			state.cancelNextFork = false;
			return { cancel: true };
		}
		return undefined;
	});

	pi.on("session_fork", (event) => {
		state.forkEvents.push({
			previousSessionFile: typeof event.previousSessionFile === "string" ? event.previousSessionFile : "",
		});
	});

	pi.on("session_before_tree", (event) => {
		state.beforeTree.push(normalizeTreePreparation(event.preparation));
		if (state.cancelNextTree) {
			state.cancelNextTree = false;
			return { cancel: true };
		}
		return undefined;
	});

	pi.on("session_tree", (event) => {
		state.treeEvents.push({
			targetId: typeof event.targetId === "string" ? event.targetId : "",
			oldLeafId: typeof event.oldLeafId === "string" ? event.oldLeafId : "",
			newLeafId: typeof event.newLeafId === "string" ? event.newLeafId : "",
		});
	});

	pi.registerCommand("armcancelswitch", {
		description: "Cancel next session switch/new",
		handler: async () => {
			state.cancelNextSwitch = true;
			return "cancel-switch-armed";
		},
	});

	pi.registerCommand("armcancelfork", {
		description: "Cancel next fork",
		handler: async () => {
			state.cancelNextFork = true;
			return "cancel-fork-armed";
		},
	});

	pi.registerCommand("armcanceltree", {
		description: "Cancel next tree navigation",
		handler: async () => {
			state.cancelNextTree = true;
			return "cancel-tree-armed";
		},
	});

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
			const sessionPath = String(args || "").trim();
			await ctx.switchSession(sessionPath);
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

	pi.registerCommand("navigateopts", {
		description: "Navigate to target with options",
		handler: async (args, ctx) => {
			const targetId = String(args || "").trim();
			await ctx.navigateTree(targetId, {
				summarize: true,
				customInstructions: "parity-tree-custom",
				replaceInstructions: true,
				label: "parity-tree-label",
			});
			return "navigate-opts-ok";
		},
	});

	pi.registerCommand("sessioninfo", {
		description: "Read command context session fields",
		handler: async (_args, ctx) => {
			return `${ctx.sessionId}|${ctx.sessionFile}|${pi.getSessionName() || ""}`;
		},
	});

	pi.registerCommand("eventsdump", {
		description: "Dump observed session events",
		handler: async () => {
			return JSON.stringify({
				beforeSwitch: state.beforeSwitch,
				switchEvents: state.switchEvents,
				beforeFork: state.beforeFork,
				forkEvents: state.forkEvents,
				beforeTree: state.beforeTree,
				treeEvents: state.treeEvents,
			});
		},
	});
}
