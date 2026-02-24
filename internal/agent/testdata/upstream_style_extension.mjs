export default function extension(pi) {
	let sawModelSelect = false;
	let sawMessageEventShape = false;
	let sawToolExecutionShape = false;
	let sawToolCallShape = false;

	pi.registerFlag("fixture-mode", {
		type: "string",
		default: "off",
		description: "Fixture behavior mode",
	});
	pi.registerFlag("fixture-base-url", {
		type: "string",
		default: "",
		description: "Fixture provider base URL",
	});

	pi.registerCommand("ping", {
		description: "Fixture ping command",
		handler: async (args) => `pong:${args}:${pi.getFlag("fixture-mode")}`,
	});
	pi.registerCommand("diag", {
		description: "Fixture diagnostics",
		handler: async () =>
			JSON.stringify({
				sawModelSelect,
				sawMessageEventShape,
				sawToolExecutionShape,
				sawToolCallShape,
			}),
	});

	pi.registerProvider("fixture-provider", {
		api: "openai-completions",
		baseUrl: pi.getFlag("fixture-base-url"),
		apiKey: "fixture-key",
		models: [
			{
				id: "fixture-model",
				name: "fixture-model",
				provider: "fixture-provider",
				api: "openai-completions",
				baseUrl: pi.getFlag("fixture-base-url"),
				reasoning: false,
				input: ["text"],
				contextWindow: 8192,
				maxTokens: 512,
			},
		],
	});

	pi.registerTool({
		name: "fixture_tool",
		label: "fixture_tool",
		description: "Fixture extension tool",
		parameters: {
			type: "object",
			properties: {
				text: { type: "string" },
			},
			required: ["text"],
		},
		execute: async (_toolCallId, params) => {
			return {
				content: [{ type: "text", text: `fixture-tool:${params.text}` }],
				details: { source: "fixture" },
			};
		},
	});

	pi.on("input", (event) => {
		if (pi.getFlag("fixture-mode") === "transform") {
			return { action: "transform", text: `${event.text} [fixture-input]` };
		}
	});

	pi.on("context", (event) => {
		return { systemPrompt: `${event.systemPrompt} [fixture-context]` };
	});

	pi.on("model_select", (event) => {
		if (event?.model?.id === "fixture-model" && event?.source === "set") {
			sawModelSelect = true;
		}
	});

	pi.on("message_start", (event) => {
		if (event?.message?.role) {
			sawMessageEventShape = true;
		}
	});

	pi.on("tool_execution_start", (event) => {
		if (event?.toolCallId && event?.args && event?.toolName) {
			sawToolExecutionShape = true;
		}
	});

	pi.on("tool_call", (event) => {
		if (event?.toolName === "fixture_tool" && event?.toolCallId && event?.input?.text === "from-provider") {
			sawToolCallShape = true;
		}
	});

	pi.on("tool_result", () => {
		return {
			content: [
				{
					type: "text",
					text: sawToolCallShape ? "fixture tool result override" : "fixture tool result missing tool_call shape",
				},
			],
			isError: false,
		};
	});
}
