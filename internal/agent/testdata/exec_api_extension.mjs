export default function extension(pi) {
	pi.registerMessageRenderer("noop", () => undefined);

	pi.registerCommand("execdiag", {
		description: "Run exec API diagnostic command",
		handler: async () => {
			const result = await pi.exec("pwd", []);
			return JSON.stringify({
				code: Number.isFinite(result?.code) ? result.code : -1,
				killed: Boolean(result?.killed),
				hasStdout: String(result?.stdout || "").trim().length > 0,
			});
		},
	});
}
