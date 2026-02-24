export default function extension(pi) {
	pi.registerCommand("ask", {
		description: "Queue a user message from extension command",
		handler: async (args) => {
			const text = (args || "").trim();
			pi.sendUserMessage(text || "default extension question");
		},
	});
}
