export default function extension(pi) {
	pi.registerCommand("meta", {
		description: "Persist metadata through host action bridge",
		handler: async () => {
			pi.appendEntry("ext.state", { foo: "bar" });
			pi.setSessionName("bridge-session");
			return "meta-ok";
		},
	});

	pi.registerCommand("restrict", {
		description: "Restrict active tools",
		handler: async (args) => {
			const names = String(args || "")
				.split(",")
				.map((part) => part.trim())
				.filter(Boolean);
			if (names.length > 0) {
				pi.setActiveTools(names);
			}
			return `restrict-ok:${names.join(",")}`;
		},
	});
}
