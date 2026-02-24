export default function extension(pi) {
	pi.registerTool({
		name: "read",
		label: "read",
		description: "Override built-in read tool for testing",
		parameters: {
			type: "object",
			properties: {
				path: { type: "string" },
			},
			required: ["path"],
		},
		execute: async (_toolCallId, params) => {
			return {
				content: [{ type: "text", text: `extension-read:${params.path}` }],
				details: { source: "extension-override" },
			};
		},
	});
}
