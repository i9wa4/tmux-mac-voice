const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const app = fs.readFileSync(`${__dirname}/web/app.js`, "utf8");
const worklet = fs.readFileSync(`${__dirname}/web/worklet.js`, "utf8");
const reply = { type: "reply", session_id: "session", turn_id: "turn", request_id: "request",
  result_id: "result", generation: 1, text: "返事", audio: "AA==" };

function browser(decodeAudioData) {
  const sent = [];
  const elements = new Map();
  const document = {
    hidden: false,
    querySelector(selector) {
      if (!elements.has(selector)) elements.set(selector, { disabled: false, textContent: "",
        addEventListener() {}, append() {} });
      return elements.get(selector);
    },
    createElement() { return { textContent: "" }; },
    addEventListener() {},
  };
  const socket = { readyState: 1, send(data) { sent.push(JSON.parse(data)); }, close() {} };
  const audio = { decodeAudioData, close: async () => {},
    createBufferSource() { throw new Error("stale playback started"); } };
  const context = vm.createContext({ document, WebSocket: { OPEN: 1 }, atob, setTimeout,
    testSocket: socket, testAudio: audio, testCapture: { port: { postMessage() {} } } });
  vm.runInContext(app, context);
  vm.runInContext(`active = true; sessionID = "session"; generation = 1;
    pending = { turnID: "turn", requestID: "request", generation: 1 };
    socket = testSocket; audioContext = testAudio; capture = testCapture;`, context);
  return { context, sent, elements };
}

test("invalid base64 aborts playback with all correlation IDs", async () => {
  const b = browser(() => Promise.resolve({}));
  vm.runInContext(`onEvent(${JSON.stringify({ ...reply, audio: "!!!" })})`, b.context);
  assert.equal(b.sent[0].type, "playback_failed");
  assert.deepEqual(b.sent[0], { type: "playback_failed", session_id: "session", turn_id: "turn",
    request_id: "request", result_id: "result", generation: 1 });
  assert.equal(b.sent[1].type, "stop");
});

test("decode rejection sends playback_failed and closes session", async () => {
  const b = browser(() => Promise.reject(new Error("decode failed")));
  vm.runInContext(`onEvent(${JSON.stringify(reply)})`, b.context);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(b.sent[0].type, "playback_failed");
  assert.equal(b.sent[0].result_id, "result");
  assert.equal(b.sent[1].type, "stop");
});

test("delayed decode after Stop cannot start stale playback", async () => {
  let resolveDecode;
  const b = browser(() => new Promise(resolve => { resolveDecode = resolve; }));
  vm.runInContext(`onEvent(${JSON.stringify(reply)})`, b.context);
  vm.runInContext("stop()", b.context);
  resolveDecode({});
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(b.sent[0].type, "abort");
  assert.equal(b.sent[0].result_id, "result");
  assert.equal(b.sent[1].type, "stop");
});

test("worklet credit exhaustion overflows once and pause/resume resets capture", () => {
  let Processor;
  const context = vm.createContext({ AudioWorkletProcessor: class { constructor() { this.port = { onmessage: null, postMessage: msg => sent.push(msg) }; } },
    registerProcessor(_name, ctor) { Processor = ctor; }, sampleRate: 16000, Int16Array, Math });
  const sent = [];
  vm.runInContext(worklet, context);
  const p = new Processor();
  p.port.onmessage({ data: { type: "resume", generation: 1 } });
  for (let i = 0; i < 5; i++) p.process([[new Float32Array(320).fill(0.2)]]);
  assert.equal(sent.filter(x => x.audio).length, 4);
  assert.equal(sent.filter(x => x.type === "overflow").length, 1);
  assert.equal(p.enabled, false);
  p.port.onmessage({ data: { type: "credit", generation: 1 } });
  p.process([[new Float32Array(320).fill(0.2)]]);
  assert.equal(sent.length, 5);
  p.port.onmessage({ data: { type: "pause" } });
  assert.equal(p.position, 0);
  assert.equal(p.count, 0);
  assert.equal(p.credits, 0);
  p.port.onmessage({ data: { type: "resume", generation: 2 } });
  p.port.onmessage({ data: { type: "credit", generation: 1 } });
  p.process([[new Float32Array(320).fill(0.2)]]);
  assert.equal(sent.at(-1).generation, 2);
  assert.equal(p.credits, 3);
});

test("mismatched reply correlation causes no playback or state advance", () => {
  for (const change of [
    { session_id: "other" }, { turn_id: "other" }, { request_id: "other" },
    { result_id: "" }, { generation: 2 },
  ]) {
    let decodes = 0;
    const b = browser(() => { decodes++; return Promise.resolve({}); });
    vm.runInContext(`onEvent(${JSON.stringify({ ...reply, ...change })})`, b.context);
    assert.equal(decodes, 0);
    assert.equal(b.sent.length, 0);
    assert.equal(vm.runInContext("pending.resultID", b.context), undefined);
    assert.equal(b.elements.get("#history").textContent, "");
  }
});
