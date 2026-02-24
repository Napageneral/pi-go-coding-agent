function extractText(content) {
	if (!Array.isArray(content)) return "";
	return content
		.filter((part) => part && typeof part === "object" && part.type === "text")
		.map((part) => (typeof part.text === "string" ? part.text : ""))
		.filter((text) => text.length > 0)
		.join("\n");
}

export default function extension(pi) {
	pi.registerCommand("newwithsetup", {
		description: "Create a new session using ctx.newSession setup callback",
		handler: async (_args, ctx) => {
			const result = await ctx.newSession({
				parentSession: ctx.sessionFile || "",
				setup: async (sessionManager) => {
					const setupMsgID = sessionManager.appendMessage({
						role: "user",
						content: [{ type: "text", text: "setup-message-user" }],
						timestamp: Date.now(),
					});
					sessionManager.appendLabelChange(setupMsgID, "setup-label");
					sessionManager.appendSessionInfo("setup-session-name");
					sessionManager.appendCustomMessageEntry(
						"setup.custom",
						[{ type: "text", text: "setup-custom-context" }],
						true,
						{ source: "new_session_setup_extension" },
					);
				},
			});
			return result?.cancelled ? "newwithsetup-cancelled" : "newwithsetup-ok";
		},
	});

	pi.registerCommand("setupdiag", {
		description: "Inspect setup-applied session state",
		handler: async (_args, ctx) => {
			const entries = ctx.sessionManager.getEntries();
			let setupMessageId = "";
			let setupMessageSeen = false;
			let setupCustomSeen = false;
			for (const entry of entries) {
				if (entry.type === "message" && entry.message?.role === "user") {
					const text = extractText(entry.message.content);
					if (text.includes("setup-message-user")) {
						setupMessageSeen = true;
						setupMessageId = entry.id;
					}
				}
				if (entry.type === "custom_message") {
					const text = extractText(entry.content);
					if (text.includes("setup-custom-context")) {
						setupCustomSeen = true;
					}
				}
			}
			return JSON.stringify({
				sessionName: ctx.sessionManager.getSessionName() || "",
				setupMessageSeen,
				setupCustomSeen,
				setupLabel: setupMessageId ? ctx.sessionManager.getLabel(setupMessageId) || "" : "",
			});
		},
	});
}
