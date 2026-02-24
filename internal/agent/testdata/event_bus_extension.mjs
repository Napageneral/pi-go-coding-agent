export default function extension(pi) {
	let state = "unset";

	pi.events.on("topic", (data) => {
		if (typeof data === "string") {
			state = data;
			return;
		}
		if (data && typeof data === "object" && typeof data.value === "string") {
			state = data.value;
		}
	});

	pi.registerCommand("emitbus", {
		description: "Emit event bus payload",
		handler: async (args) => {
			pi.events.emit("topic", { value: String(args || "").trim() });
			return "emitbus-ok";
		},
	});

	pi.registerCommand("busstate", {
		description: "Read event bus state",
		handler: async () => `busstate:${state}`,
	});
}
