const state = {
	completed: false,
	summary: "",
	error: "",
};

function resetState() {
	state.completed = false;
	state.summary = "";
	state.error = "";
}

export default function extension(pi) {
	pi.registerCommand("compactcbfail", {
		description: "Trigger compact with callbacks expecting failure",
		handler: async (_args, ctx) => {
			resetState();
			ctx.compact({
				onComplete: (result) => {
					state.completed = true;
					state.summary = result?.summary || "";
				},
				onError: (error) => {
					state.error = error?.message || String(error);
				},
			});
			return "compactcbfail-armed";
		},
	});

	pi.registerCommand("compactcbok", {
		description: "Trigger compact with callbacks expecting success",
		handler: async (args, ctx) => {
			resetState();
			ctx.compact({
				customInstructions: String(args || "").trim() || undefined,
				onComplete: (result) => {
					state.completed = true;
					state.summary = result?.summary || "";
				},
				onError: (error) => {
					state.error = error?.message || String(error);
				},
			});
			return "compactcbok-armed";
		},
	});

	pi.registerCommand("compactcbstate", {
		description: "Read compact callback state",
		handler: async () => JSON.stringify(state),
	});
}
