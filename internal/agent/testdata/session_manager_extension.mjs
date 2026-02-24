function hasReadonlySessionManagerShape(sm) {
	const required = [
		"getCwd",
		"getSessionDir",
		"getSessionId",
		"getSessionFile",
		"getLeafId",
		"getLeafEntry",
		"getEntry",
		"getLabel",
		"getBranch",
		"getHeader",
		"getEntries",
		"getTree",
		"getSessionName",
	];
	return required.every((name) => typeof sm?.[name] === "function");
}

export default function extension(pi) {
	pi.registerCommand("smdiag", {
		description: "Session manager diagnostics",
		handler: async (_args, ctx) => {
			const sm = ctx.sessionManager;
			const leafId = sm.getLeafId();
			const leafEntry = leafId ? sm.getLeafEntry() : undefined;
			const header = sm.getHeader();
			const branch = sm.getBranch();
			const tree = sm.getTree();
			const entries = sm.getEntries();
			return JSON.stringify({
				hasShape: hasReadonlySessionManagerShape(sm),
				cwd: sm.getCwd(),
				sessionDir: sm.getSessionDir(),
				sessionId: sm.getSessionId(),
				sessionFile: sm.getSessionFile() || "",
				sessionName: sm.getSessionName() || "",
				leafId: leafId || "",
				leafEntryId: leafEntry?.id || "",
				headerId: header?.id || "",
				entryByLeafId: leafId ? sm.getEntry(leafId)?.id || "" : "",
				entriesCount: entries.length,
				branchCount: branch.length,
				treeRoots: tree.length,
			});
		},
	});

	pi.registerCommand("smlabel", {
		description: "Set label on current leaf",
		handler: async (_args, ctx) => {
			const targetId = ctx.sessionManager.getLeafId();
			if (!targetId) return "smlabel-none";
			pi.setLabel(targetId, "smdiag-label");
			return `smlabel-ok:${targetId}`;
		},
	});

	pi.registerCommand("smgetlabel", {
		description: "Get label for entry id",
		handler: async (args, ctx) => {
			const id = String(args || "").trim();
			if (!id) return "";
			return ctx.sessionManager.getLabel(id) || "";
		},
	});

	pi.registerCommand("smname", {
		description: "Set session name",
		handler: async () => {
			pi.setSessionName("smdiag-session");
			return "smname-ok";
		},
	});
}
