#!/usr/bin/env node

import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import path from "node:path";
import process from "node:process";
import readline from "node:readline";
import { pathToFileURL } from "node:url";

const DEFAULT_PROTOCOL_VERSION = "2026-02-24";
const SIDECAR_VERSION = "0.1.0";

class SidecarError extends Error {
	constructor(code, message) {
		super(message);
		this.code = code;
	}
}

const options = parseArgs(process.argv.slice(2));
const state = {
	initialized: false,
	handlers: new Map(),
	tools: new Map(),
	flags: new Map(),
	pendingFlagValues: {},
	commands: new Map(),
	shortcuts: new Map(),
	providers: [],
	loadedExtensions: new Set(),
	loadingExtensionPath: "",
	cwd: "",
	sessionId: "",
	sessionFile: "",
	sessionDir: "",
	sessionName: "",
	leafId: "",
	sessionHeader: null,
	sessionEntries: [],
	sessionById: new Map(),
	sessionLabels: new Map(),
	model: undefined,
	allModels: [],
	availableModels: [],
	providerApiKeys: {},
	providerAuthTypes: {},
	contextSystemPrompt: "",
	contextThinkingLevel: "medium",
	contextIsIdle: true,
	contextHasPendingMessages: false,
	contextUsage: undefined,
	hostTools: new Map(),
	activeTools: new Set(),
	eventBusHandlers: new Map(),
	compactCallbacks: new Map(),
	pendingUIRequests: new Map(),
	hasUI: false,
};
let currentActionSink = null;

const noOpUI = {
	select: async () => undefined,
	confirm: async () => false,
	input: async () => undefined,
	notify: () => {},
	onTerminalInput: () => () => {},
	setStatus: () => {},
	setWorkingMessage: () => {},
	setWidget: () => {},
	setFooter: () => {},
	setHeader: () => {},
	setTitle: () => {},
	custom: async () => undefined,
	pasteToEditor: () => {},
	setEditorText: () => {},
	getEditorText: () => "",
	editor: async () => undefined,
	setEditorComponent: () => {},
	get theme() {
		return {};
	},
	getAllThemes: () => [],
	getTheme: () => undefined,
	setTheme: () => ({ success: false, error: "UI not available" }),
	getToolsExpanded: () => false,
	setToolsExpanded: () => {},
};

function emitNotification(payload) {
	process.stdout.write(`${JSON.stringify(payload)}\n`);
}

function createDialogPromise(method, payload, options, defaultValue, parseResponse) {
	if (!state.hasUI) {
		return Promise.resolve(defaultValue);
	}

	const opts = asObject(options);
	const signal = opts.signal;
	if (signal && typeof signal === "object" && signal.aborted) {
		return Promise.resolve(defaultValue);
	}

	const timeout = Number(opts.timeout);
	const timeoutMs = Number.isFinite(timeout) && timeout > 0 ? Math.trunc(timeout) : 0;
	const id = randomUUID().slice(0, 8);

	return new Promise((resolve) => {
		let timeoutId;
		let settled = false;

		const cleanup = () => {
			if (timeoutId) {
				clearTimeout(timeoutId);
			}
			if (signal && typeof signal.removeEventListener === "function") {
				signal.removeEventListener("abort", onAbort);
			}
			state.pendingUIRequests.delete(id);
		};

		const settle = (value) => {
			if (settled) return;
			settled = true;
			cleanup();
			resolve(value);
		};

		const onAbort = () => settle(defaultValue);

		if (signal && typeof signal.addEventListener === "function") {
			signal.addEventListener("abort", onAbort, { once: true });
		}

		if (timeoutMs > 0) {
			timeoutId = setTimeout(() => settle(defaultValue), timeoutMs);
		}

		state.pendingUIRequests.set(id, {
			resolve: (response) => {
				try {
					settle(parseResponse(asObject(response)));
				} catch {
					settle(defaultValue);
				}
			},
		});

		emitNotification({
			type: "extension_ui_request",
			id,
			method,
			...payload,
			...(timeoutMs > 0 ? { timeout: timeoutMs } : {}),
		});
	});
}

const rl = readline.createInterface({
	input: process.stdin,
	crlfDelay: Infinity,
});

const queue = [];
let processing = false;

rl.on("line", (line) => {
	if (!line || !line.trim()) return;
	queue.push(line);
	if (!processing) {
		processing = true;
		void processQueue();
	}
});

rl.on("close", () => {
	process.exit(0);
});

async function processQueue() {
	try {
		while (queue.length > 0) {
			const line = queue.shift();
			if (!line) continue;
			await handleLine(line);
		}
	} finally {
		processing = false;
		if (queue.length > 0) {
			processing = true;
			void processQueue();
		}
	}
}

async function handleLine(line) {
	let req;
	try {
		req = JSON.parse(line);
	} catch {
		return;
	}
	if (!req || typeof req !== "object" || typeof req.method !== "string" || req.id === undefined || req.id === null) {
		return;
	}
	const id = req.id;
	try {
		const result = await dispatch(req.method, req.params ?? {});
		respond(id, { result });
		if (req.method === "shutdown") {
			process.exit(0);
		}
	} catch (err) {
		respond(id, { error: normalizeError(err) });
	}
}

async function dispatch(method, params) {
	switch (method) {
		case "initialize":
			return initialize(params);
		case "emit":
			return emit(params);
		case "tool.execute":
			return executeTool(params);
		case "command.execute":
			return executeCommand(params);
		case "ui.respond":
			return uiRespond(params);
		case "shutdown":
			return { ok: true };
		default:
			throw new SidecarError("method_not_found", `Unknown method: ${method}`);
	}
}

async function initialize(params) {
	const requestProtocolVersion = readString(params.protocolVersion, DEFAULT_PROTOCOL_VERSION);
	const extensionPaths = uniqueStrings([...options.extensions, ...readStringArray(params.extensionPaths)]);
	state.cwd = readString(params.cwd, "");
	state.sessionId = readString(params.sessionId, "");
	state.sessionFile = readString(params.sessionFile, "");
	state.sessionDir = readString(params.sessionDir, "");
	state.sessionName = readString(params.sessionName, "");
	state.leafId = readString(params.leafId, "");
	applySessionSnapshot({
		sessionHeader: params.sessionHeader,
		sessionEntries: params.sessionEntries,
		leafId: state.leafId,
		fallbackCwd: state.cwd,
	});
	syncContextSnapshot(params);
	state.hostTools = new Map(readToolArray(params.hostTools).map((tool) => [tool.name, tool]));
	const initialActiveTools = readStringArray(params.activeTools);
	state.activeTools = new Set(
		initialActiveTools.length > 0 ? initialActiveTools : [...state.hostTools.keys()],
	);
	state.pendingFlagValues = asObject(params.flagValues);
	state.hasUI = readBool(params.hasUI, false);
	await loadExtensions(extensionPaths);
	applyFlagValues(params.flagValues);
	state.initialized = true;
	return {
		protocolVersion: requestProtocolVersion,
		sidecarVersion: SIDECAR_VERSION,
		capabilities: ["events", "tools", "commands", "flags", "providers"],
		tools: [...state.tools.values()].map((tool) => tool.definition),
		flags: [...state.flags.values()].map((flag) => ({
			name: flag.name,
			description: flag.description,
			type: flag.type,
			default: flag.default,
		})),
		commands: [...state.commands.values()].map((command) => ({
			name: command.name,
			description: command.description,
			path: command.extensionPath || undefined,
		})),
		providers: state.providers.map((provider) => ({
			name: provider.name,
			config: provider.config,
		})),
	};
}

async function emit(params) {
	if (!state.initialized) {
		throw new SidecarError("not_initialized", "initialize must be called before emit");
	}
	const eventObj = asObject(params.event);
	const eventType = readString(eventObj.type, "");
	if (!eventType) {
		throw new SidecarError("invalid_request", "event.type is required");
	}
	const payload = asObject(eventObj.payload);
	syncHostStateFromEvent(eventType, payload);
	const handlers = state.handlers.get(eventType) ?? [];
	if (handlers.length === 0) {
		return {};
	}
	const actions = [];

	switch (eventType) {
		case "input":
			return emitInput(handlers, payload, actions);
		case "before_agent_start":
			return emitBeforeAgentStart(handlers, payload, actions);
		case "context":
			return emitContext(handlers, payload, actions);
		case "tool_call":
			return emitToolCall(handlers, payload, actions);
		case "tool_result":
			return emitToolResult(handlers, payload, actions);
		case "session_before_switch":
			return emitSessionBeforeSwitch(handlers, payload, actions);
		case "session_before_fork":
			return emitSessionBeforeFork(handlers, payload, actions);
		case "session_before_compact":
			return emitSessionBeforeCompact(handlers, payload, actions);
		case "session_before_tree":
			return emitSessionBeforeTree(handlers, payload, actions);
		default:
			await runHandlers(eventType, handlers, payload, actions);
			return withActionsResponse({}, actions);
	}
}

async function executeTool(params) {
	if (!state.initialized) {
		throw new SidecarError("not_initialized", "initialize must be called before tool execution");
	}
	const name = readString(params.name, "");
	if (!name) {
		throw new SidecarError("invalid_request", "tool name is required");
	}
	const tool = state.tools.get(name);
	if (!tool) {
		throw new SidecarError("tool_not_found", `tool ${name} is not registered`);
	}
	const toolCallID = readString(params.toolCallID, "");
	const args = asObject(params.arguments);
	const raw = await invokeToolExecute(tool.execute, toolCallID, args);
	return normalizeToolResult(raw);
}

async function executeCommand(params) {
	if (!state.initialized) {
		throw new SidecarError("not_initialized", "initialize must be called before command execution");
	}
	syncContextSnapshot(params);
	const name = readString(params.name, "").trim();
	const args = readString(params.args, "");
	if (!name) {
		throw new SidecarError("invalid_request", "command name is required");
	}
	const command = state.commands.get(name);
	if (!command) {
		return { handled: false };
	}
	const actions = [];
	const out = await invokeWithActionSink(() => command.handler(args, createCommandContext()), actions);
	if (typeof out === "string") {
		return withActionsResponse({ handled: true, output: out }, actions);
	}
	if (out && typeof out === "object") {
		return withActionsResponse(
			{
			handled: out.handled !== false,
			output: readString(out.output, ""),
			},
			actions,
		);
	}
	return withActionsResponse({ handled: true }, actions);
}

async function uiRespond(params) {
	const id = readString(params.id, "").trim();
	if (!id) {
		throw new SidecarError("invalid_request", "ui.respond requires id");
	}
	const pending = state.pendingUIRequests.get(id);
	if (!pending) {
		return { resolved: false };
	}
	state.pendingUIRequests.delete(id);
	if (typeof pending.resolve === "function") {
		pending.resolve(asObject(params));
	}
	return { resolved: true };
}

async function emitInput(handlers, payload, actions) {
	let action = "pass";
	let text = readString(payload.text, "");
	let assistantText = "";
	const event = { type: "input", ...payload };

	for (const handler of handlers) {
		const out = await invokeHandlerSafely("input", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (out.action === "handled") {
			action = "handled";
			assistantText = readString(out.assistantText, assistantText);
			break;
		}
		if (out.action === "transform") {
			action = "transform";
			text = readString(out.text, text);
		}
	}

	if (action === "pass") return withActionsResponse({}, actions);
	const result = { action };
	if (action === "transform") result.text = text;
	if (action === "handled" && assistantText) result.assistantText = assistantText;
	return withActionsResponse({ input: result }, actions);
}

async function emitBeforeAgentStart(handlers, payload, actions) {
	const event = { type: "before_agent_start", ...payload };
	const basePrompt = readString(payload.systemPrompt, "");
	let systemPrompt = basePrompt;
	const messages = [];

	for (const handler of handlers) {
		const out = await invokeHandlerSafely("before_agent_start", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (typeof out.systemPrompt === "string") {
			systemPrompt = out.systemPrompt;
		}
		if (out.message && typeof out.message === "object") {
			messages.push(normalizeMessage(out.message));
		}
	}

	if (messages.length === 0 && systemPrompt === basePrompt) return withActionsResponse({}, actions);
	return withActionsResponse({
		beforeAgentStart: {
			systemPrompt,
			messages,
		},
	}, actions);
}

async function emitContext(handlers, payload, actions) {
	const event = { type: "context", ...payload };
	const basePrompt = readString(payload.systemPrompt, "");
	let systemPrompt = basePrompt;
	let messages = normalizeMessages(payload.messages);
	let changed = false;

	for (const handler of handlers) {
		const out = await invokeHandlerSafely("context", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (typeof out.systemPrompt === "string") {
			systemPrompt = out.systemPrompt;
			changed = true;
		}
		if (Array.isArray(out.messages)) {
			messages = normalizeMessages(out.messages);
			changed = true;
		}
	}

	if (!changed) return withActionsResponse({}, actions);
	return withActionsResponse({
		context: {
			systemPrompt,
			messages,
		},
	}, actions);
}

async function emitToolCall(handlers, payload, actions) {
	const event = { type: "tool_call", ...payload };
	for (const handler of handlers) {
		const out = await invokeHandlerSafely("tool_call", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (out.block === true) {
			return withActionsResponse({
				toolCall: {
					block: true,
					reason: readString(out.reason, ""),
				},
			}, actions);
		}
	}
	return withActionsResponse({}, actions);
}

async function emitToolResult(handlers, payload, actions) {
	const event = { type: "tool_result", ...payload };
	let content = normalizeContent(payload.content);
	let details = payload.details;
	let isError = Boolean(payload.isError);
	let changed = false;

	for (const handler of handlers) {
		const out = await invokeHandlerSafely("tool_result", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (Object.prototype.hasOwnProperty.call(out, "content")) {
			content = normalizeContent(out.content);
			changed = true;
		}
		if (Object.prototype.hasOwnProperty.call(out, "details")) {
			details = out.details;
			changed = true;
		}
		if (Object.prototype.hasOwnProperty.call(out, "isError")) {
			isError = Boolean(out.isError);
			changed = true;
		}
	}

	if (!changed) return withActionsResponse({}, actions);
	return withActionsResponse({
		toolResult: {
			content,
			details,
			isError,
		},
	}, actions);
}

async function emitSessionBeforeSwitch(handlers, payload, actions) {
	const event = { type: "session_before_switch", ...payload };
	let cancel = false;
	for (const handler of handlers) {
		const out = await invokeHandlerSafely("session_before_switch", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (out.cancel === true) {
			cancel = true;
			break;
		}
	}
	if (!cancel) return withActionsResponse({}, actions);
	return withActionsResponse({
		sessionBeforeSwitch: {
			cancel: true,
		},
	}, actions);
}

async function emitSessionBeforeFork(handlers, payload, actions) {
	const event = { type: "session_before_fork", ...payload };
	let cancel = false;
	let skipConversationRestore = false;
	for (const handler of handlers) {
		const out = await invokeHandlerSafely("session_before_fork", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (out.skipConversationRestore === true) {
			skipConversationRestore = true;
		}
		if (out.cancel === true) {
			cancel = true;
			break;
		}
	}
	if (!cancel && !skipConversationRestore) return withActionsResponse({}, actions);
	return withActionsResponse({
		sessionBeforeFork: {
			cancel,
			skipConversationRestore,
		},
	}, actions);
}

function normalizeCompactionOverride(raw, payload) {
	const obj = asObject(raw);
	const summary = readString(obj.summary, "").trim();
	if (!summary) return undefined;
	const preparation = asObject(payload.preparation);
	let firstKeptEntryId = readString(obj.firstKeptEntryId, "").trim();
	if (!firstKeptEntryId) {
		firstKeptEntryId = readString(preparation.firstKeptEntryId, "").trim();
	}
	let tokensBefore = Number(obj.tokensBefore);
	if (!Number.isFinite(tokensBefore)) {
		tokensBefore = Number(preparation.tokensBefore);
	}
	if (!Number.isFinite(tokensBefore) || tokensBefore < 0) {
		tokensBefore = 0;
	}
	return {
		summary,
		firstKeptEntryId,
		tokensBefore: Math.trunc(tokensBefore),
		details: asObject(obj.details),
	};
}

async function emitSessionBeforeCompact(handlers, payload, actions) {
	const event = { type: "session_before_compact", ...payload };
	let cancel = false;
	let compaction;
	for (const handler of handlers) {
		const out = await invokeHandlerSafely("session_before_compact", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (out.cancel === true) {
			cancel = true;
			break;
		}
		if (out.compaction && typeof out.compaction === "object") {
			const override = normalizeCompactionOverride(out.compaction, payload);
			if (override) {
				compaction = override;
			}
		}
	}
	if (!cancel && !compaction) return withActionsResponse({}, actions);
	const response = { cancel };
	if (compaction) {
		response.compaction = compaction;
	}
	return withActionsResponse({ sessionBeforeCompact: response }, actions);
}

async function emitSessionBeforeTree(handlers, payload, actions) {
	const event = { type: "session_before_tree", ...payload };
	let cancel = false;
	let customInstructions = "";
	let replaceInstructions = false;
	let hasReplaceInstructions = false;
	let label = "";
	let hasLabel = false;
	let summary;

	for (const handler of handlers) {
		const out = await invokeHandlerSafely("session_before_tree", handler, event, actions);
		if (!out || typeof out !== "object") continue;
		if (out.cancel === true) {
			cancel = true;
			break;
		}
		if (typeof out.customInstructions === "string") {
			customInstructions = out.customInstructions;
		}
		if (Object.prototype.hasOwnProperty.call(out, "replaceInstructions")) {
			replaceInstructions = Boolean(out.replaceInstructions);
			hasReplaceInstructions = true;
		}
		if (Object.prototype.hasOwnProperty.call(out, "label")) {
			label = out.label == null ? "" : String(out.label);
			hasLabel = true;
		}
		if (out.summary && typeof out.summary === "object") {
			summary = out.summary;
		}
	}

	const response = {};
	if (cancel) {
		response.cancel = true;
	}
	if (customInstructions !== "") {
		response.customInstructions = customInstructions;
	}
	if (hasReplaceInstructions) {
		response.replaceInstructions = replaceInstructions;
	}
	if (hasLabel) {
		response.label = label;
	}
	if (summary && typeof summary === "object") {
		response.summary = summary;
	}
	if (Object.keys(response).length === 0) return withActionsResponse({}, actions);
	return withActionsResponse({ sessionBeforeTree: response }, actions);
}

async function runHandlers(eventType, handlers, payload, actions) {
	const event = { type: eventType, ...payload };
	for (const handler of handlers) {
		await invokeHandlerSafely(eventType, handler, event, actions);
	}
}

function syncHostStateFromEvent(eventType, payload) {
	syncContextSnapshot(payload);
	switch (eventType) {
		case "session_start":
			state.sessionId = readString(payload.sessionId, state.sessionId);
			state.sessionFile = readString(payload.sessionFile, state.sessionFile);
			state.sessionDir = readString(payload.sessionDir, state.sessionDir);
			state.sessionName = readString(payload.sessionName, state.sessionName);
			state.leafId = readString(payload.leafId, state.leafId);
			applySessionSnapshot({
				sessionHeader: payload.sessionHeader,
				sessionEntries: payload.sessionEntries,
				leafId: state.leafId,
				fallbackCwd: readString(payload.cwd, state.cwd),
			});
			if (Array.isArray(payload.hostTools)) {
				state.hostTools = new Map(readToolArray(payload.hostTools).map((tool) => [tool.name, tool]));
			}
			if (Array.isArray(payload.activeTools)) {
				const nextActive = readStringArray(payload.activeTools);
				state.activeTools = new Set(
					nextActive.length > 0 ? nextActive : [...state.hostTools.keys(), ...state.tools.keys()],
				);
			}
			break;
		case "message_end":
			appendSessionEntry(payload.entry);
			break;
		case "session_tree": {
			const nextLeaf = readString(payload.newLeafId, "");
			const summaryEntry = asObject(payload.summaryEntry);
			const summaryID = readString(summaryEntry.id, "").trim();
			const summaryText = readString(summaryEntry.summary, "").trim();
			if (summaryID && summaryText) {
				appendSessionEntry({
					type: "branch_summary",
					id: summaryID,
					parentId: readString(payload.targetId, "").trim() || null,
					timestamp: new Date().toISOString(),
					fromId: readString(payload.oldLeafId, "").trim(),
					summary: summaryText,
					details: asObject(summaryEntry.details),
					fromHook: true,
				});
			}
			if (nextLeaf) {
				state.leafId = nextLeaf;
				break;
			}
			const targetId = readString(payload.targetId, "");
			if (targetId) {
				state.leafId = targetId;
			}
			break;
		}
		case "session_compact": {
			appendSessionEntry(payload.compactionEntry);
			const requestId = readString(payload.requestId, "").trim();
			if (!requestId) break;
			const callbacks = state.compactCallbacks.get(requestId);
			if (!callbacks) break;
			state.compactCallbacks.delete(requestId);
			if (typeof callbacks.onComplete === "function") {
				const entry = asObject(payload.compactionEntry);
				const result = {
					summary: readString(entry.summary, ""),
					firstKeptEntryId: readString(entry.firstKeptEntryId, ""),
					tokensBefore: Number.isFinite(Number(entry.tokensBefore)) ? Number(entry.tokensBefore) : 0,
					details: asObject(entry.details),
				};
				Promise.resolve()
					.then(() => callbacks.onComplete(result))
					.catch((err) => {
						console.error("Extension compact onComplete callback error:", err);
					});
			}
			break;
		}
		case "session_compact_error": {
			const requestId = readString(payload.requestId, "").trim();
			if (!requestId) break;
			const callbacks = state.compactCallbacks.get(requestId);
			if (!callbacks) break;
			state.compactCallbacks.delete(requestId);
			if (typeof callbacks.onError === "function") {
				const message = readString(payload.error, "Compaction failed");
				Promise.resolve()
					.then(() => callbacks.onError(new Error(message)))
					.catch((err) => {
						console.error("Extension compact onError callback error:", err);
					});
			}
			break;
		}
		case "agent_start":
			state.contextIsIdle = false;
			break;
		case "agent_end":
			state.contextIsIdle = true;
			break;
		default:
			break;
	}
}

function createExtensionContext() {
	const rpcUI = {
		select: (title, options, opts) =>
			createDialogPromise(
				"select",
				{
					title: readString(title, ""),
					options: Array.isArray(options)
						? options
								.map((option) => {
									if (typeof option === "string") return option;
									return readString(option?.label, "");
								})
								.filter((option) => option !== "")
						: [],
				},
				opts,
				undefined,
				(response) => {
					if (response.cancelled === true) return undefined;
					const value = readString(response.value, "");
					return value === "" ? undefined : value;
				},
			),
		confirm: (title, message, opts) =>
			createDialogPromise(
				"confirm",
				{
					title: readString(title, ""),
					message: readString(message, ""),
				},
				opts,
				false,
				(response) => {
					if (response.cancelled === true) return false;
					return typeof response.confirmed === "boolean" ? response.confirmed : false;
				},
			),
		input: (title, placeholder, opts) =>
			createDialogPromise(
				"input",
				{
					title: readString(title, ""),
					placeholder: readString(placeholder, ""),
				},
				opts,
				undefined,
				(response) => {
					if (response.cancelled === true) return undefined;
					const value = readString(response.value, "");
					return value === "" ? undefined : value;
				},
			),
		notify: (message, type = "info") => {
			emitNotification({
				type: "extension_ui_request",
				id: randomUUID().slice(0, 8),
				method: "notify",
				message: readString(message, ""),
				notifyType: readString(type, "info"),
			});
		},
		onTerminalInput: () => () => {},
		setStatus: (key, text) => {
			emitNotification({
				type: "extension_ui_request",
				id: randomUUID().slice(0, 8),
				method: "setStatus",
				statusKey: readString(key, ""),
				statusText: text == null ? "" : readString(text, ""),
			});
		},
		setWorkingMessage: () => {},
		setWidget: (key, content, options = {}) => {
			if (!(content === undefined || Array.isArray(content))) {
				return;
			}
			emitNotification({
				type: "extension_ui_request",
				id: randomUUID().slice(0, 8),
				method: "setWidget",
				widgetKey: readString(key, ""),
				widgetLines: Array.isArray(content)
					? content.map((line) => readString(line, "")).filter((line) => line !== "")
					: undefined,
				widgetPlacement: readString(asObject(options).placement, ""),
			});
		},
		setFooter: () => {},
		setHeader: () => {},
		setTitle: (title) => {
			emitNotification({
				type: "extension_ui_request",
				id: randomUUID().slice(0, 8),
				method: "setTitle",
				title: readString(title, ""),
			});
		},
		custom: async () => undefined,
		pasteToEditor: (text) => {
			emitNotification({
				type: "extension_ui_request",
				id: randomUUID().slice(0, 8),
				method: "set_editor_text",
				text: readString(text, ""),
			});
		},
		setEditorText: (text) => {
			emitNotification({
				type: "extension_ui_request",
				id: randomUUID().slice(0, 8),
				method: "set_editor_text",
				text: readString(text, ""),
			});
		},
		getEditorText: () => "",
		editor: (title, prefill, opts) =>
			createDialogPromise(
				"editor",
				{
					title: readString(title, ""),
					prefill: readString(prefill, ""),
				},
				opts,
				undefined,
				(response) => {
					if (response.cancelled === true) return undefined;
					const value = readString(response.value, "");
					return value === "" ? undefined : value;
				},
			),
		setEditorComponent: () => {},
		get theme() {
			return {};
		},
		getAllThemes: () => [],
		getTheme: () => undefined,
		setTheme: () => ({ success: false, error: "UI not available" }),
		getToolsExpanded: () => false,
		setToolsExpanded: () => {},
	};

	return {
		ui: state.hasUI ? rpcUI : noOpUI,
		hasUI: state.hasUI,
		cwd: state.cwd,
		sessionManager: createReadonlySessionManager(),
		modelRegistry: createModelRegistryView(),
		get model() {
			return state.model ? deepClone(state.model) : undefined;
		},
		isIdle: () => state.contextIsIdle,
		abort: () => pushHostAction({ type: "abort" }),
		hasPendingMessages: () => state.contextHasPendingMessages,
		shutdown: () => pushHostAction({ type: "shutdown" }),
		getContextUsage: () => (state.contextUsage ? deepClone(state.contextUsage) : undefined),
		compact: (options = {}) => {
			const opts = asObject(options);
			const requestId = randomUUID().slice(0, 8);
			const onComplete = typeof opts.onComplete === "function" ? opts.onComplete : undefined;
			const onError = typeof opts.onError === "function" ? opts.onError : undefined;
			if (onComplete || onError) {
				state.compactCallbacks.set(requestId, { onComplete, onError });
			}
			pushHostAction({
				type: "compact",
				requestId,
				customInstructions: readString(opts.customInstructions, ""),
			});
		},
		getSystemPrompt: () => state.contextSystemPrompt,
	};
}

function createCommandContext() {
	return {
		...createExtensionContext(),
		getFlag: (flagName) => getFlagValue(readString(flagName, "")),
		flags: Object.fromEntries(
			[...state.flags.values()].map((flag) => [
				flag.name,
				Object.prototype.hasOwnProperty.call(flag, "value") ? flag.value : flag.default,
			]),
		),
		sessionId: state.sessionId,
		sessionFile: state.sessionFile,
		waitForIdle: async () => {
			pushHostAction({ type: "wait_for_idle" });
		},
		newSession: async (options = {}) => {
			const opts = asObject(options);
			const before = await runLocalSessionBeforeSwitch("new", "");
			if (before.cancel) {
				return { cancelled: true };
			}
			const setupEntries = [];
			if (typeof opts.setup === "function") {
				const setupManager = createSetupSessionManager(setupEntries);
				await opts.setup(setupManager);
			}
			pushHostAction({
				type: "new_session",
				parentSession: readString(opts.parentSession, ""),
				setupEntries,
				skipBeforeHooks: true,
			});
			return { cancelled: false };
		},
		fork: async (entryId) => {
			const target = readString(entryId, "").trim();
			if (!target) return { cancelled: true };
			const before = await runLocalSessionBeforeFork(target);
			if (before.cancel) {
				return { cancelled: true };
			}
			pushHostAction({
				type: "fork",
				entryId: target,
				skipBeforeHooks: true,
			});
			return { cancelled: false };
		},
		navigateTree: async (targetId, options = {}) => {
			const target = readString(targetId, "").trim();
			if (!target) return { cancelled: true };
			const opts = asObject(options);
			const before = await runLocalSessionBeforeTree(target, opts);
			if (before.cancel) {
				return { cancelled: true };
			}
			const customInstructions = before.customInstructions ?? readString(opts.customInstructions, "");
			const replaceInstructions = before.replaceInstructions ?? readBool(opts.replaceInstructions, false);
			const label = before.label ?? readString(opts.label, "");
			pushHostAction({
				type: "navigate_tree",
				targetId: target,
				summarize: readBool(opts.summarize, false),
				customInstructions,
				replaceInstructions,
				label,
				skipBeforeHooks: true,
				summary: readString(before.summary?.summary, ""),
				summaryDetails: asObject(before.summary?.details),
			});
			return { cancelled: false };
		},
		switchSession: async (sessionPath) => {
			const path = readString(sessionPath, "").trim();
			if (!path) return { cancelled: true };
			const before = await runLocalSessionBeforeSwitch("resume", path);
			if (before.cancel) {
				return { cancelled: true };
			}
			pushHostAction({
				type: "switch_session",
				sessionPath: path,
				skipBeforeHooks: true,
			});
			return { cancelled: false };
		},
		reload: async () => {
			pushHostAction({ type: "reload" });
		},
	};
}

async function runLocalSessionBeforeSwitch(reason, targetSessionFile) {
	const handlers = state.handlers.get("session_before_switch") ?? [];
	if (handlers.length === 0) return { cancel: false };
	const event = { type: "session_before_switch", reason };
	if (targetSessionFile) {
		event.targetSessionFile = targetSessionFile;
	}
	for (const handler of handlers) {
		const out = await invokeHandlerSafely("session_before_switch", handler, event, currentActionSink);
		if (!out || typeof out !== "object") continue;
		if (out.cancel === true) {
			return { cancel: true };
		}
	}
	return { cancel: false };
}

async function runLocalSessionBeforeFork(entryId) {
	const handlers = state.handlers.get("session_before_fork") ?? [];
	if (handlers.length === 0) return { cancel: false, skipConversationRestore: false };
	let skipConversationRestore = false;
	const event = { type: "session_before_fork", entryId };
	for (const handler of handlers) {
		const out = await invokeHandlerSafely("session_before_fork", handler, event, currentActionSink);
		if (!out || typeof out !== "object") continue;
		if (out.skipConversationRestore === true) {
			skipConversationRestore = true;
		}
		if (out.cancel === true) {
			return { cancel: true, skipConversationRestore };
		}
	}
	return { cancel: false, skipConversationRestore };
}

async function runLocalSessionBeforeTree(targetId, options) {
	const handlers = state.handlers.get("session_before_tree") ?? [];
	const preparation = {
		targetId,
		oldLeafId: state.leafId || "",
		userWantsSummary: readBool(options.summarize, false),
		customInstructions: readString(options.customInstructions, ""),
		replaceInstructions: readBool(options.replaceInstructions, false),
		label: readString(options.label, ""),
	};
	if (handlers.length === 0) {
		return {
			cancel: false,
			customInstructions: undefined,
			replaceInstructions: undefined,
			label: undefined,
			summary: undefined,
		};
	}
	let customInstructions;
	let replaceInstructions;
	let label;
	let summary;
	const event = { type: "session_before_tree", preparation };
	for (const handler of handlers) {
		const out = await invokeHandlerSafely("session_before_tree", handler, event, currentActionSink);
		if (!out || typeof out !== "object") continue;
		if (out.cancel === true) {
			return { cancel: true, customInstructions, replaceInstructions, label, summary };
		}
		if (typeof out.customInstructions === "string") {
			customInstructions = out.customInstructions;
		}
		if (Object.prototype.hasOwnProperty.call(out, "replaceInstructions")) {
			replaceInstructions = Boolean(out.replaceInstructions);
		}
		if (Object.prototype.hasOwnProperty.call(out, "label")) {
			label = out.label == null ? "" : String(out.label);
		}
		if (out.summary && typeof out.summary === "object") {
			summary = {
				summary: readString(out.summary.summary, ""),
				details: asObject(out.summary.details),
			};
		}
	}
	return { cancel: false, customInstructions, replaceInstructions, label, summary };
}

function createSetupSessionManager(setupEntries) {
	const knownRefs = new Set();

	const nextRef = () => {
		let ref = "";
		do {
			ref = randomUUID().slice(0, 8);
		} while (knownRefs.has(ref));
		knownRefs.add(ref);
		return ref;
	};

	const resolveTarget = (targetId) => {
		const target = readString(targetId, "").trim();
		if (!target) return {};
		if (knownRefs.has(target)) {
			return { targetRef: target };
		}
		return { targetId: target };
	};

	const appendSetupEntry = (entry) => {
		setupEntries.push(entry);
	};

	return {
		getCwd: () => state.cwd,
		getSessionDir: () => state.sessionDir || (state.sessionFile ? path.dirname(state.sessionFile) : ""),
		getSessionId: () => state.sessionId,
		getSessionFile: () => state.sessionFile || undefined,
		getSessionName: () => state.sessionName || undefined,
		appendMessage(message) {
			const ref = nextRef();
			appendSetupEntry({
				op: "append_message",
				ref,
				message: normalizeMessage(message),
			});
			return ref;
		},
		appendThinkingLevelChange(thinkingLevel) {
			const level = readString(thinkingLevel, "").trim();
			if (!level) return "";
			const ref = nextRef();
			appendSetupEntry({
				op: "append_thinking_level_change",
				ref,
				thinkingLevel: level,
			});
			return ref;
		},
		appendModelChange(provider, modelId) {
			const p = readString(provider, "").trim();
			const id = readString(modelId, "").trim();
			if (!p || !id) return "";
			const ref = nextRef();
			appendSetupEntry({
				op: "append_model_change",
				ref,
				provider: p,
				modelId: id,
			});
			return ref;
		},
		appendCustomEntry(customType, data) {
			const ct = readString(customType, "").trim();
			if (!ct) return "";
			const ref = nextRef();
			appendSetupEntry({
				op: "append_custom_entry",
				ref,
				customType: ct,
				data: data && typeof data === "object" ? data : undefined,
			});
			return ref;
		},
		appendCustomMessageEntry(customType, content, display = true, details = undefined) {
			const ct = readString(customType, "").trim();
			if (!ct) return "";
			let normalizedContent = [];
			if (typeof content === "string") {
				const text = content.trim();
				if (text) normalizedContent = [{ type: "text", text }];
			} else {
				normalizedContent = normalizeContent(content);
			}
			if (normalizedContent.length === 0) return "";
			const ref = nextRef();
			appendSetupEntry({
				op: "append_custom_message",
				ref,
				customType: ct,
				content: normalizedContent,
				display: Boolean(display),
				details: details && typeof details === "object" ? details : undefined,
			});
			return ref;
		},
		appendSessionInfo(name) {
			const value = readString(name, "").trim();
			if (!value) return "";
			const ref = nextRef();
			appendSetupEntry({
				op: "append_session_info",
				ref,
				name: value,
			});
			return ref;
		},
		appendLabelChange(targetId, label) {
			const target = resolveTarget(targetId);
			if (!target.targetId && !target.targetRef) return "";
			const ref = nextRef();
			appendSetupEntry({
				op: "append_label",
				ref,
				...target,
				label: label == null ? "" : String(label),
			});
			return ref;
		},
	};
}

function createReadonlySessionManager() {
	return {
		getCwd: () => (state.sessionHeader && typeof state.sessionHeader.cwd === "string" ? state.sessionHeader.cwd : state.cwd),
		getSessionDir: () => state.sessionDir || (state.sessionFile ? path.dirname(state.sessionFile) : ""),
		getSessionId: () => state.sessionId,
		getSessionFile: () => state.sessionFile || undefined,
		getLeafId: () => (state.leafId ? state.leafId : null),
		getLeafEntry: () => {
			if (!state.leafId) return undefined;
			const entry = state.sessionById.get(state.leafId);
			return entry ? deepClone(entry) : undefined;
		},
		getEntry: (id) => {
			const key = readString(id, "").trim();
			if (!key) return undefined;
			const entry = state.sessionById.get(key);
			return entry ? deepClone(entry) : undefined;
		},
		getLabel: (id) => {
			const key = readString(id, "").trim();
			if (!key) return undefined;
			return state.sessionLabels.get(key);
		},
		getBranch: (fromId) => {
			const startId = readString(fromId, "").trim() || state.leafId;
			return deepClone(computeBranch(startId));
		},
		getHeader: () => (state.sessionHeader ? deepClone(state.sessionHeader) : null),
		getEntries: () => deepClone(state.sessionEntries),
		getTree: () => deepClone(buildSessionTree(state.sessionEntries)),
		getSessionName: () => (state.sessionName || undefined),
	};
}

function createModelRegistryView() {
	const getAllModels = () => (state.allModels.length > 0 ? state.allModels : state.availableModels);
	return {
		getAll: () => deepClone(getAllModels()),
		getAvailable: () => deepClone(state.availableModels),
		find: (provider, modelId) => {
			const found = findModel(provider, modelId);
			return found ? deepClone(found) : undefined;
		},
		getApiKey: async (model) => {
			const m = asObject(model);
			const provider = readString(m.provider, "");
			return getProviderAPIKey(provider);
		},
		getApiKeyForProvider: async (provider) => getProviderAPIKey(provider),
		getApiKeyByProvider: async (provider) => getProviderAPIKey(provider),
		isUsingOAuth: (model) => {
			const m = asObject(model);
			const provider = readString(m.provider, "").trim();
			return getProviderAuthType(provider) === "oauth";
		},
		refresh: () => {},
		getError: () => undefined,
	};
}

async function invokeWithActionSink(fn, actions) {
	const previousSink = currentActionSink;
	currentActionSink = actions;
	try {
		return await fn();
	} finally {
		currentActionSink = previousSink;
	}
}

async function invokeHandlerSafely(eventType, handler, event, actions) {
	try {
		return await invokeWithActionSink(() => handler(event, createExtensionContext()), actions);
	} catch (err) {
		console.error(`Extension handler error (${eventType}):`, err);
		return undefined;
	}
}

function pushHostAction(action) {
	if (!currentActionSink) {
		throw new SidecarError(
			"invalid_extension",
			"Action API methods can only be called while processing an extension event or command",
		);
	}
	currentActionSink.push(action);
}

function withActionsResponse(result, actions) {
	if (!Array.isArray(actions) || actions.length === 0) {
		return result;
	}
	return { ...result, actions };
}

async function loadExtensions(paths) {
	for (const rawPath of paths) {
		const resolved = path.resolve(rawPath);
		if (state.loadedExtensions.has(resolved)) continue;
		if (!existsSync(resolved)) {
			throw new SidecarError("extension_not_found", `Extension not found: ${resolved}`);
		}
		const extension = await import(pathToFileURL(resolved).href);
		const factory = resolveExtensionFactory(extension);
		const api = createExtensionAPI();
		state.loadingExtensionPath = resolved;
		try {
			await factory(api);
		} finally {
			state.loadingExtensionPath = "";
		}
		state.loadedExtensions.add(resolved);
	}
}

function createExtensionAPI() {
	return {
		on(event, handler) {
			if (typeof event !== "string" || event.trim() === "") {
				throw new SidecarError("invalid_extension", "event name must be a non-empty string");
			}
			if (typeof handler !== "function") {
				throw new SidecarError("invalid_extension", "event handler must be a function");
			}
			const list = state.handlers.get(event) ?? [];
			list.push(handler);
			state.handlers.set(event, list);
		},
		registerTool(tool) {
			const def = normalizeToolDefinition(tool);
			if (state.tools.has(def.name)) {
				throw new SidecarError("invalid_extension", `tool ${def.name} is already registered`);
			}
			state.tools.set(def.name, {
				definition: {
					name: def.name,
					label: def.label,
					description: def.description,
					parameters: def.parameters,
				},
				execute: def.execute,
			});
			state.activeTools.add(def.name);
		},
		registerFlag(name, options = {}) {
			name = readString(name, "").trim();
			if (!name) {
				throw new SidecarError("invalid_extension", "registerFlag requires a non-empty name");
			}
			const type = readString(options.type, "string");
			if (type !== "string" && type !== "boolean") {
				throw new SidecarError("invalid_extension", `flag ${name} has invalid type ${type}`);
			}
			const flag = {
				name,
				description: readString(options.description, ""),
				type,
				default: Object.prototype.hasOwnProperty.call(options, "default")
					? coerceFlagValue(type, options.default)
					: type === "boolean"
						? false
						: "",
			};
			if (state.flags.has(name)) {
				const existing = state.flags.get(name);
				flag.value = Object.prototype.hasOwnProperty.call(existing, "value") ? existing.value : flag.default;
			} else if (Object.prototype.hasOwnProperty.call(state.pendingFlagValues, name)) {
				flag.value = coerceFlagValue(type, state.pendingFlagValues[name]);
			}
			state.flags.set(name, flag);
		},
		getFlag(name) {
			return getFlagValue(readString(name, ""));
		},
		registerCommand(name, options = {}) {
			name = readString(name, "").trim();
			if (!name) {
				throw new SidecarError("invalid_extension", "registerCommand requires a non-empty name");
			}
			if (typeof options.handler !== "function") {
				throw new SidecarError("invalid_extension", `command ${name} is missing handler()`);
			}
			state.commands.set(name, {
				name,
				description: readString(options.description, ""),
				extensionPath: state.loadingExtensionPath || undefined,
				handler: options.handler,
			});
		},
		registerShortcut(shortcut, options = {}) {
			const key = readString(shortcut, "").toLowerCase();
			if (!key) {
				throw new SidecarError("invalid_extension", "registerShortcut requires a shortcut key");
			}
			state.shortcuts.set(key, {
				shortcut: key,
				description: readString(options.description, ""),
				handler: options.handler,
			});
		},
		registerMessageRenderer(_customType, _renderer) {
			// Print/CLI mode does not render custom message components.
		},
		async exec(command, args = [], options = {}) {
			const cmd = readString(command, "").trim();
			if (!cmd) {
				return {
					stdout: "",
					stderr: "missing command",
					code: 1,
					killed: false,
				};
			}
			const argv = Array.isArray(args)
				? args.map((arg) => readString(arg, "")).filter((arg) => arg !== "")
				: [];
			const opts = asObject(options);
			const timeout = Number.isFinite(opts.timeout) ? Math.max(0, Number(opts.timeout)) : 0;
			const execCWD = readString(opts.cwd, "").trim() || state.cwd || process.cwd();
			return runExecCommand(cmd, argv, execCWD, timeout);
		},
		registerProvider(name, config = {}) {
			name = readString(name, "").trim();
			if (!name) {
				throw new SidecarError("invalid_extension", "registerProvider requires a non-empty name");
			}
			state.providers.push({
				name,
				config: asObject(config),
			});
		},
		sendUserMessage(content, options = {}) {
			const normalizedContent = normalizeUserMessageContent(content);
			const text = normalizeUserMessageText(normalizedContent);
			if (!text && normalizedContent.length === 0) return;
			pushHostAction({
				type: "send_user_message",
				text,
				content: normalizedContent,
				deliverAs: readString(options.deliverAs, ""),
			});
		},
		sendMessage(message, options = {}) {
			const msg = asObject(message);
			const text = normalizeCustomMessageText(message);
			let content = normalizeContent(msg.content);
			if (content.length === 0 && text) {
				content = [{ type: "text", text }];
			}
			const customType = readString(msg.customType, "").trim();
			if (!text && content.length === 0 && !customType) return;
			const details = asObject(msg.details);
			pushHostAction({
				type: "send_message",
				role: readString(msg.role, "assistant"),
				text,
				deliverAs: readString(options.deliverAs, ""),
				triggerTurn: options.triggerTurn === true,
				customType,
				content,
				display: readBool(msg.display, true),
				data: Object.keys(details).length > 0 ? details : undefined,
			});
			const role = readString(msg.role, "assistant");
			if (customType) {
				appendSyntheticEntry("custom_message", {
					customType,
					content,
					display: readBool(msg.display, true),
					data: Object.keys(details).length > 0 ? details : undefined,
				});
			} else if (role !== "user") {
				appendSyntheticEntry("message", {
					message: {
						role,
						content,
						timestamp: Date.now(),
					},
				});
			}
		},
		appendEntry(_customType, _data) {
			const customType = readString(_customType, "").trim();
			if (!customType) return;
			pushHostAction({
				type: "append_entry",
				customType,
				data: _data && typeof _data === "object" ? _data : undefined,
			});
			appendSyntheticEntry("custom", {
				customType,
				data: _data && typeof _data === "object" ? _data : undefined,
			});
		},
		setSessionName(name) {
			const value = readString(name, "").trim();
			if (!value) return;
			state.sessionName = value;
			pushHostAction({
				type: "set_session_name",
				name: value,
			});
			appendSyntheticEntry("session_info", {
				name: value,
			});
		},
		getSessionName() {
			return state.sessionName || undefined;
		},
		setLabel(entryId, label) {
			const targetId = readString(entryId, "").trim();
			if (!targetId) return;
			pushHostAction({
				type: "set_label",
				targetId,
				label: label == null ? "" : String(label),
			});
			appendSyntheticEntry("label", {
				targetId,
				label: label == null ? "" : String(label),
			});
		},
		getActiveTools() {
			return [...state.activeTools];
		},
		getAllTools() {
			const merged = new Map(state.hostTools);
			for (const tool of state.tools.values()) {
				merged.set(tool.definition.name, {
					name: tool.definition.name,
					description: tool.definition.description,
					parameters: tool.definition.parameters,
				});
			}
			return [...merged.values()];
		},
		setActiveTools(toolNames) {
			if (!Array.isArray(toolNames)) return;
			const names = toolNames
				.map((name) => readString(name, "").trim())
				.filter((name) => name.length > 0);
			state.activeTools = new Set(names);
			pushHostAction({
				type: "set_active_tools",
				toolNames: names,
			});
		},
		getCommands() {
			return [...state.commands.values()].map((command) => ({
				name: command.name,
				description: command.description,
				source: "extension",
				path: command.extensionPath || undefined,
			}));
		},
		async setModel(model) {
			const nextModel = normalizeModel(model);
			if (!nextModel) return false;
			const matched = findModel(nextModel.provider, nextModel.id);
			if (!matched) return false;
			state.model = deepClone(matched);
			pushHostAction({
				type: "set_model",
				provider: matched.provider,
				model: matched.id,
			});
			return true;
		},
		getThinkingLevel() {
			return state.contextThinkingLevel;
		},
		setThinkingLevel(level) {
			const normalized = readString(level, "").trim();
			if (!normalized) return;
			state.contextThinkingLevel = normalized;
			pushHostAction({
				type: "set_thinking_level",
				thinkingLevel: normalized,
			});
		},
		events: {
			on(channel, handler) {
				const key = readString(channel, "").trim();
				if (!key || typeof handler !== "function") {
					return () => {};
				}
				const list = state.eventBusHandlers.get(key) ?? [];
				list.push(handler);
				state.eventBusHandlers.set(key, list);
				return () => {
					const current = state.eventBusHandlers.get(key) ?? [];
					state.eventBusHandlers.set(
						key,
						current.filter((h) => h !== handler),
					);
				};
			},
			emit(channel, data) {
				const key = readString(channel, "").trim();
				if (!key) return;
				const handlers = state.eventBusHandlers.get(key) ?? [];
				for (const handler of handlers) {
					Promise.resolve(handler(data)).catch((err) => {
						console.error(`Extension event bus handler error (${key}):`, err);
					});
				}
			},
		},
	};
}

function normalizeToolDefinition(raw) {
	if (!raw || typeof raw !== "object") {
		throw new SidecarError("invalid_extension", "registerTool expects an object");
	}
	const name = readString(raw.name, "");
	const description = readString(raw.description, "");
	if (!name) {
		throw new SidecarError("invalid_extension", "tool.name is required");
	}
	if (!description) {
		throw new SidecarError("invalid_extension", `tool ${name} is missing description`);
	}
	if (typeof raw.execute !== "function") {
		throw new SidecarError("invalid_extension", `tool ${name} is missing execute()`);
	}
	return {
		name,
		label: readString(raw.label, ""),
		description,
		parameters: asObject(raw.parameters),
		execute: raw.execute,
	};
}

function resolveExtensionFactory(moduleValue) {
	if (typeof moduleValue?.default === "function") return moduleValue.default;
	if (typeof moduleValue?.extension === "function") return moduleValue.extension;
	if (typeof moduleValue === "function") return moduleValue;
	throw new SidecarError("invalid_extension", "Extension must export a factory function (default export)");
}

async function invokeToolExecute(execute, toolCallID, args) {
	if (execute.length <= 1) {
		return execute(args);
	}
	if (execute.length === 2) {
		return execute(toolCallID, args);
	}
	return execute(toolCallID, args, undefined, undefined, {});
}

function normalizeToolResult(raw) {
	if (typeof raw === "string") {
		return {
			content: [{ type: "text", text: raw }],
			isError: false,
		};
	}
	const obj = asObject(raw);
	return {
		content: normalizeContent(obj.content),
		details: obj.details,
		isError: Boolean(obj.isError),
	};
}

function normalizeMessage(raw) {
	const msg = asObject(raw);
	return {
		role: readString(msg.role, "user"),
		content: normalizeContent(msg.content),
		timestamp: Number.isFinite(msg.timestamp) ? msg.timestamp : Date.now(),
	};
}

function normalizeMessages(raw) {
	if (!Array.isArray(raw)) return [];
	return raw.map((message) => normalizeMessage(message));
}

function normalizeContent(raw) {
	if (!Array.isArray(raw)) {
		return [];
	}
	return raw
		.map((block) => asObject(block))
		.filter((block) => typeof block.type === "string" && block.type.length > 0)
		.map((block) => {
			if (block.type === "text") {
				return {
					type: "text",
					text: readString(block.text, ""),
				};
			}
			return block;
		});
}

function normalizeUserMessageContent(content) {
	if (typeof content === "string") {
		const text = content.trim();
		return text ? [{ type: "text", text }] : [];
	}
	if (!Array.isArray(content)) {
		return [];
	}
	const out = [];
	for (const part of content) {
		const obj = asObject(part);
		switch (readString(obj.type, "").trim()) {
			case "text": {
				const text = readString(obj.text, "").trim();
				if (text) {
					out.push({ type: "text", text });
				}
				break;
			}
			case "image": {
				const data = readString(obj.data, "").trim();
				const mimeType = readString(obj.mimeType, "").trim();
				if (data && mimeType) {
					out.push({ type: "image", data, mimeType });
				}
				break;
			}
			default:
				break;
		}
	}
	return out;
}

function normalizeUserMessageText(content) {
	const normalized = Array.isArray(content) ? content : normalizeUserMessageContent(content);
	const textParts = [];
	for (const part of normalized) {
		const obj = asObject(part);
		if (obj.type !== "text") continue;
		const text = readString(obj.text, "").trim();
		if (text) textParts.push(text);
	}
	return textParts.join("\n").trim();
}

function normalizeCustomMessageText(message) {
	if (typeof message === "string") {
		return message.trim();
	}
	const msg = asObject(message);
	if (typeof msg.content === "string") {
		return msg.content.trim();
	}
	if (Array.isArray(msg.content)) {
		const text = normalizeUserMessageText(msg.content);
		if (text) return text;
	}
	if (typeof msg.display === "string") {
		return msg.display.trim();
	}
	return "";
}

function normalizeError(err) {
	if (err instanceof SidecarError) {
		return { code: err.code, message: err.message };
	}
	if (err instanceof Error) {
		return { code: "internal_error", message: err.message };
	}
	return { code: "internal_error", message: String(err) };
}

function runExecCommand(command, args, cwd, timeoutMs) {
	return new Promise((resolve) => {
		const proc = spawn(command, args, {
			cwd,
			shell: false,
			stdio: ["ignore", "pipe", "pipe"],
		});
		let stdout = "";
		let stderr = "";
		let killed = false;
		let timeoutID;

		const killProcess = () => {
			if (killed) return;
			killed = true;
			proc.kill("SIGTERM");
			setTimeout(() => {
				if (!proc.killed) proc.kill("SIGKILL");
			}, 5000);
		};

		if (timeoutMs > 0) {
			timeoutID = setTimeout(killProcess, timeoutMs);
		}

		proc.stdout?.on("data", (chunk) => {
			stdout += String(chunk);
		});
		proc.stderr?.on("data", (chunk) => {
			stderr += String(chunk);
		});
		proc.on("close", (code) => {
			if (timeoutID) clearTimeout(timeoutID);
			resolve({
				stdout,
				stderr,
				code: Number.isFinite(code) ? code : 0,
				killed,
			});
		});
		proc.on("error", () => {
			if (timeoutID) clearTimeout(timeoutID);
			resolve({
				stdout,
				stderr,
				code: 1,
				killed,
			});
		});
	});
}

function deepClone(value) {
	if (value === undefined) return undefined;
	if (value === null) return null;
	return structuredClone(value);
}

function normalizeModel(raw) {
	const model = asObject(raw);
	const id = readString(model.id, "").trim();
	if (!id) return undefined;
	return {
		...model,
		id,
		name: readString(model.name, id),
		provider: readString(model.provider, ""),
		api: readString(model.api, ""),
		baseUrl: readString(model.baseUrl, ""),
	};
}

function readModelArray(value) {
	if (!Array.isArray(value)) return [];
	return value
		.map((item) => normalizeModel(item))
		.filter((item) => item && typeof item === "object");
}

function readProviderAPIKeys(value) {
	const obj = asObject(value);
	const out = {};
	for (const [provider, rawKey] of Object.entries(obj)) {
		const name = provider.trim();
		if (!name) continue;
		const key = typeof rawKey === "string" ? rawKey : rawKey == null ? "" : String(rawKey);
		if (!key) continue;
		out[name] = key;
	}
	return out;
}

function readProviderAuthTypes(value) {
	const obj = asObject(value);
	const out = {};
	for (const [provider, rawType] of Object.entries(obj)) {
		const name = provider.trim();
		if (!name) continue;
		const authType = readString(rawType, "").trim().toLowerCase();
		if (authType !== "oauth" && authType !== "api_key") continue;
		out[name] = authType;
	}
	return out;
}

function normalizeContextUsage(value) {
	const obj = asObject(value);
	const contextWindow = Number(obj.contextWindow);
	if (!Number.isFinite(contextWindow) || contextWindow <= 0) {
		return undefined;
	}
	let tokens = null;
	if (Object.prototype.hasOwnProperty.call(obj, "tokens")) {
		if (obj.tokens == null) {
			tokens = null;
		} else if (Number.isFinite(Number(obj.tokens))) {
			tokens = Number(obj.tokens);
		}
	}
	let percent = null;
	if (Object.prototype.hasOwnProperty.call(obj, "percent")) {
		if (obj.percent == null) {
			percent = null;
		} else if (Number.isFinite(Number(obj.percent))) {
			percent = Number(obj.percent);
		}
	}
	return {
		tokens,
		contextWindow,
		percent,
	};
}

function getProviderAPIKey(provider) {
	const name = readString(provider, "").trim();
	if (!name) return undefined;
	if (Object.prototype.hasOwnProperty.call(state.providerApiKeys, name)) {
		return state.providerApiKeys[name];
	}
	const lowered = name.toLowerCase();
	for (const [candidate, key] of Object.entries(state.providerApiKeys)) {
		if (candidate.toLowerCase() === lowered) {
			return key;
		}
	}
	return undefined;
}

function getProviderAuthType(provider) {
	const name = readString(provider, "").trim();
	if (!name) return undefined;
	if (Object.prototype.hasOwnProperty.call(state.providerAuthTypes, name)) {
		return state.providerAuthTypes[name];
	}
	const lowered = name.toLowerCase();
	for (const [candidate, authType] of Object.entries(state.providerAuthTypes)) {
		if (candidate.toLowerCase() === lowered) {
			return authType;
		}
	}
	return undefined;
}

function findModel(provider, modelId) {
	const p = readString(provider, "").trim().toLowerCase();
	const id = readString(modelId, "").trim().toLowerCase();
	if (!p || !id) return undefined;
	const models = state.allModels.length > 0 ? state.allModels : state.availableModels;
	return models.find((model) => {
		const providerName = readString(model?.provider, "").trim().toLowerCase();
		const candidateID = readString(model?.id, "").trim().toLowerCase();
		return providerName === p && candidateID === id;
	});
}

function syncContextSnapshot(raw) {
	const obj = asObject(raw);
	const ctxModel = normalizeModel(obj.ctxModel);
	if (ctxModel) {
		state.model = ctxModel;
	}
	const currentModel = normalizeModel(obj.currentModel);
	if (currentModel) {
		state.model = currentModel;
	}
	if (Array.isArray(obj.ctxAllModels)) {
		state.allModels = readModelArray(obj.ctxAllModels);
	} else if (Array.isArray(obj.allModels)) {
		state.allModels = readModelArray(obj.allModels);
	}
	if (Array.isArray(obj.ctxAvailableModels)) {
		state.availableModels = readModelArray(obj.ctxAvailableModels);
	} else if (Array.isArray(obj.availableModels)) {
		state.availableModels = readModelArray(obj.availableModels);
	}
	if (obj.ctxProviderApiKeys && typeof obj.ctxProviderApiKeys === "object") {
		state.providerApiKeys = readProviderAPIKeys(obj.ctxProviderApiKeys);
	} else if (obj.providerApiKeys && typeof obj.providerApiKeys === "object") {
		state.providerApiKeys = readProviderAPIKeys(obj.providerApiKeys);
	}
	if (obj.ctxProviderAuthTypes && typeof obj.ctxProviderAuthTypes === "object") {
		state.providerAuthTypes = readProviderAuthTypes(obj.ctxProviderAuthTypes);
	} else if (obj.providerAuthTypes && typeof obj.providerAuthTypes === "object") {
		state.providerAuthTypes = readProviderAuthTypes(obj.providerAuthTypes);
	}
	if (typeof obj.ctxSystemPrompt === "string") {
		state.contextSystemPrompt = obj.ctxSystemPrompt;
	} else if (typeof obj.systemPrompt === "string") {
		state.contextSystemPrompt = obj.systemPrompt;
	}
	if (typeof obj.ctxThinkingLevel === "string") {
		state.contextThinkingLevel = obj.ctxThinkingLevel;
	} else if (typeof obj.thinkingLevel === "string") {
		state.contextThinkingLevel = obj.thinkingLevel;
	}
	if (typeof obj.ctxIsIdle === "boolean") {
		state.contextIsIdle = obj.ctxIsIdle;
	} else if (typeof obj.isIdle === "boolean") {
		state.contextIsIdle = obj.isIdle;
	}
	if (typeof obj.ctxHasPendingMessages === "boolean") {
		state.contextHasPendingMessages = obj.ctxHasPendingMessages;
	} else if (typeof obj.hasPendingMessages === "boolean") {
		state.contextHasPendingMessages = obj.hasPendingMessages;
	}
	const ctxUsage = normalizeContextUsage(obj.ctxContextUsage);
	if (ctxUsage) {
		state.contextUsage = ctxUsage;
	} else {
		const usage = normalizeContextUsage(obj.contextUsage);
		if (usage) {
			state.contextUsage = usage;
		}
	}
}

function normalizeSessionHeader(raw, fallbackCwd) {
	const obj = asObject(raw);
	const header = {
		type: "session",
		version: Number.isFinite(obj.version) ? Number(obj.version) : undefined,
		id: readString(obj.id, state.sessionId),
		timestamp: readString(obj.timestamp, ""),
		cwd: readString(obj.cwd, fallbackCwd || state.cwd),
		parentSession: readString(obj.parentSession, ""),
	};
	if (!header.id) header.id = state.sessionId;
	if (!header.cwd) header.cwd = fallbackCwd || state.cwd || "";
	if (!header.parentSession) delete header.parentSession;
	if (!Number.isFinite(header.version)) delete header.version;
	return header;
}

function normalizeSessionEntry(raw) {
	const obj = asObject(raw);
	const id = readString(obj.id, "").trim();
	const type = readString(obj.type, "").trim();
	if (!id || !type) return null;
	let parentId = null;
	if (Object.prototype.hasOwnProperty.call(obj, "parentId")) {
		if (obj.parentId == null) {
			parentId = null;
		} else {
			const pid = readString(obj.parentId, "").trim();
			parentId = pid || null;
		}
	}
	return {
		...obj,
		id,
		type,
		parentId,
		timestamp: readString(obj.timestamp, ""),
	};
}

function recomputeSessionIndexes() {
	state.sessionById = new Map();
	state.sessionLabels = new Map();
	let latestSessionName = "";
	for (const entry of state.sessionEntries) {
		state.sessionById.set(entry.id, entry);
		if (entry.type === "label") {
			const targetId = readString(entry.targetId, "").trim();
			if (!targetId) continue;
			const label = Object.prototype.hasOwnProperty.call(entry, "label")
				? entry.label == null
					? ""
					: String(entry.label)
				: "";
			if (label) {
				state.sessionLabels.set(targetId, label);
			} else {
				state.sessionLabels.delete(targetId);
			}
		}
		if (entry.type === "session_info") {
			const name = readString(entry.name, "").trim();
			if (name) latestSessionName = name;
		}
	}
	if (latestSessionName) {
		state.sessionName = latestSessionName;
	}
}

function applySessionSnapshot({ sessionHeader, sessionEntries, leafId, fallbackCwd }) {
	state.sessionHeader = normalizeSessionHeader(sessionHeader, fallbackCwd || state.cwd);
	if (state.sessionHeader.id) {
		state.sessionId = state.sessionHeader.id;
	}
	if (state.sessionHeader.cwd) {
		state.cwd = state.sessionHeader.cwd;
	}
	if (Array.isArray(sessionEntries)) {
		state.sessionEntries = sessionEntries
			.map((entry) => normalizeSessionEntry(entry))
			.filter((entry) => entry && typeof entry === "object");
	}
	recomputeSessionIndexes();
	const nextLeaf = readString(leafId, "").trim();
	if (nextLeaf) {
		state.leafId = nextLeaf;
	} else if (state.sessionEntries.length > 0) {
		state.leafId = state.sessionEntries[state.sessionEntries.length - 1].id;
	}
	if (!state.sessionDir && state.sessionFile) {
		state.sessionDir = path.dirname(state.sessionFile);
	}
}

function appendSessionEntry(rawEntry) {
	const entry = normalizeSessionEntry(rawEntry);
	if (!entry) return;
	const existing = state.sessionById.get(entry.id);
	if (existing) {
		const idx = state.sessionEntries.findIndex((e) => e.id === entry.id);
		if (idx >= 0) state.sessionEntries[idx] = entry;
	} else {
		state.sessionEntries.push(entry);
	}
	recomputeSessionIndexes();
	state.leafId = entry.id;
}

function computeBranch(fromId) {
	const pathEntries = [];
	if (!fromId) return pathEntries;
	const seen = new Set();
	let current = state.sessionById.get(fromId);
	while (current && !seen.has(current.id)) {
		seen.add(current.id);
		pathEntries.unshift(current);
		if (!current.parentId) break;
		current = state.sessionById.get(current.parentId);
	}
	return pathEntries;
}

function buildSessionTree(entries) {
	const nodeMap = new Map();
	const roots = [];
	for (const entry of entries) {
		nodeMap.set(entry.id, {
			entry,
			children: [],
			label: state.sessionLabels.get(entry.id),
		});
	}
	for (const entry of entries) {
		const node = nodeMap.get(entry.id);
		if (!entry.parentId || entry.parentId === entry.id) {
			roots.push(node);
			continue;
		}
		const parent = nodeMap.get(entry.parentId);
		if (!parent) {
			roots.push(node);
			continue;
		}
		parent.children.push(node);
	}
	const stack = [...roots];
	while (stack.length > 0) {
		const node = stack.pop();
		node.children.sort((a, b) => {
			const ta = Date.parse(readString(a.entry.timestamp, "")) || 0;
			const tb = Date.parse(readString(b.entry.timestamp, "")) || 0;
			return ta - tb;
		});
		for (const child of node.children) {
			stack.push(child);
		}
	}
	return roots;
}

function appendSyntheticEntry(type, patch = {}) {
	const parentId = state.leafId || null;
	const entry = normalizeSessionEntry({
		type,
		id: randomUUID().slice(0, 8),
		parentId,
		timestamp: new Date().toISOString(),
		...patch,
	});
	if (!entry) return;
	appendSessionEntry(entry);
}

function applyFlagValues(rawValues) {
	const values = asObject(rawValues);
	for (const [name, rawValue] of Object.entries(values)) {
		const flag = state.flags.get(name);
		if (!flag) continue;
		flag.value = coerceFlagValue(flag.type, rawValue);
		state.flags.set(name, flag);
	}
}

function getFlagValue(name) {
	const flag = state.flags.get(name);
	if (!flag) {
		if (Object.prototype.hasOwnProperty.call(state.pendingFlagValues, name)) {
			return state.pendingFlagValues[name];
		}
		return undefined;
	}
	if (Object.prototype.hasOwnProperty.call(flag, "value")) {
		return flag.value;
	}
	return flag.default;
}

function coerceFlagValue(type, value) {
	if (type === "boolean") {
		if (typeof value === "boolean") return value;
		if (typeof value === "string") {
			const normalized = value.trim().toLowerCase();
			if (normalized === "true" || normalized === "1" || normalized === "yes" || normalized === "on") return true;
			if (normalized === "false" || normalized === "0" || normalized === "no" || normalized === "off") return false;
		}
		return Boolean(value);
	}
	if (typeof value === "string") return value;
	if (value == null) return "";
	return String(value);
}

function respond(id, payload) {
	const response = { id };
	if (payload.error) {
		response.error = payload.error;
	} else {
		response.result = payload.result ?? {};
	}
	process.stdout.write(`${JSON.stringify(response)}\n`);
}

function parseArgs(argv) {
	const extensions = [];
	for (let i = 0; i < argv.length; i++) {
		const arg = argv[i];
		if (arg === "--extension" && i + 1 < argv.length) {
			extensions.push(argv[i + 1]);
			i++;
		}
	}
	return { extensions };
}

function asObject(value) {
	return value && typeof value === "object" ? value : {};
}

function readString(value, fallback) {
	return typeof value === "string" ? value : fallback;
}

function readBool(value, fallback) {
	return typeof value === "boolean" ? value : fallback;
}

function readStringArray(value) {
	if (!Array.isArray(value)) return [];
	return value.filter((item) => typeof item === "string");
}

function readToolArray(value) {
	if (!Array.isArray(value)) return [];
	return value
		.map((item) => asObject(item))
		.filter((item) => typeof item.name === "string" && item.name.trim() !== "")
		.map((item) => ({
			name: readString(item.name, "").trim(),
			description: readString(item.description, ""),
			parameters: asObject(item.parameters),
		}));
}

function uniqueStrings(values) {
	return [...new Set(values.filter((value) => typeof value === "string" && value.trim() !== ""))];
}
