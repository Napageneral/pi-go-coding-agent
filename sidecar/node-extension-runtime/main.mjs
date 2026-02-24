#!/usr/bin/env node

import { spawn } from "node:child_process";
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
	cwd: "",
	sessionId: "",
	sessionFile: "",
	sessionName: "",
	hostTools: new Map(),
	activeTools: new Set(),
	eventBusHandlers: new Map(),
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
	state.sessionName = readString(params.sessionName, "");
	state.hostTools = new Map(readToolArray(params.hostTools).map((tool) => [tool.name, tool]));
	const initialActiveTools = readStringArray(params.activeTools);
	state.activeTools = new Set(
		initialActiveTools.length > 0 ? initialActiveTools : [...state.hostTools.keys()],
	);
	state.pendingFlagValues = asObject(params.flagValues);
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
	switch (eventType) {
		case "session_start":
			state.sessionId = readString(payload.sessionId, state.sessionId);
			state.sessionFile = readString(payload.sessionFile, state.sessionFile);
			state.sessionName = readString(payload.sessionName, state.sessionName);
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
		default:
			break;
	}
}

function createExtensionContext() {
	return {
		ui: noOpUI,
		hasUI: false,
		cwd: state.cwd,
		sessionManager: {},
		modelRegistry: {},
		model: undefined,
		isIdle: () => true,
		abort: () => pushHostAction({ type: "abort" }),
		hasPendingMessages: () => false,
		shutdown: () => pushHostAction({ type: "shutdown" }),
		getContextUsage: () => undefined,
		compact: () => {},
		getSystemPrompt: () => "",
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
			pushHostAction({
				type: "new_session",
				parentSession: readString(opts.parentSession, ""),
			});
			return { cancelled: false };
		},
		fork: async (entryId) => {
			const target = readString(entryId, "").trim();
			if (!target) return { cancelled: true };
			pushHostAction({
				type: "fork",
				entryId: target,
			});
			return { cancelled: false };
		},
		navigateTree: async (targetId, options = {}) => {
			const target = readString(targetId, "").trim();
			if (!target) return { cancelled: true };
			const opts = asObject(options);
			pushHostAction({
				type: "navigate_tree",
				targetId: target,
				summarize: readBool(opts.summarize, false),
				customInstructions: readString(opts.customInstructions, ""),
				replaceInstructions: readBool(opts.replaceInstructions, false),
				label: readString(opts.label, ""),
			});
			return { cancelled: false };
		},
		switchSession: async (sessionPath) => {
			const path = readString(sessionPath, "").trim();
			if (!path) return { cancelled: true };
			state.sessionFile = path;
			pushHostAction({
				type: "switch_session",
				sessionPath: path,
			});
			return { cancelled: false };
		},
		reload: async () => {
			pushHostAction({ type: "reload" });
		},
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
		await factory(api);
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
			const text = normalizeUserMessageText(content);
			if (!text) return;
			pushHostAction({
				type: "send_user_message",
				text,
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
		},
		appendEntry(_customType, _data) {
			const customType = readString(_customType, "").trim();
			if (!customType) return;
			pushHostAction({
				type: "append_entry",
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
			}));
		},
		async setModel(model) {
			const m = asObject(model);
			const modelId = readString(m.id, "");
			if (!modelId) return false;
			pushHostAction({
				type: "set_model",
				provider: readString(m.provider, ""),
				model: modelId,
			});
			return true;
		},
		getThinkingLevel() {
			return "medium";
		},
		setThinkingLevel(level) {
			const normalized = readString(level, "").trim();
			if (!normalized) return;
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

function normalizeUserMessageText(content) {
	if (typeof content === "string") {
		return content.trim();
	}
	if (!Array.isArray(content)) {
		return "";
	}
	const textParts = [];
	for (const part of content) {
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
