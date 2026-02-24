export default function extension(pi) {
	pi.registerCommand("ask", {
		description: "Queue a user message from extension command",
		handler: async (args) => {
			const text = (args || "").trim();
			pi.sendUserMessage(text || "default extension question");
		},
	});

	pi.registerCommand("askwithimage", {
		description: "Queue a user message with text + image blocks",
		handler: async (args) => {
			const text = (args || "").trim() || "describe the image";
			pi.sendUserMessage([
				{ type: "text", text },
				{ type: "image", data: "aGVsbG8=", mimeType: "image/png" },
			]);
		},
	});
}
