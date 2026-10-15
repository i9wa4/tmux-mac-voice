const tokenInput = document.querySelector("#token");
const startButton = document.querySelector("#start");
const stopButton = document.querySelector("#stop");
const status = document.querySelector("#status");
const history = document.querySelector("#history");

let socket;
let media;
let audioContext;
let capture;
let playback;
let active = false;
let listening = false;
let sessionID = "";
let generation = 0;
let pending = null;
let epoch = 0;

function setStatus(text) { status.textContent = text; }
function appendLine(label, value) {
  const line = document.createElement("p");
  line.textContent = `${label}: ${value}`;
  history.append(line);
}
function gate() {
  listening = false;
  capture?.port.postMessage({ type: "pause" });
}

function playbackControl(type, turn = pending) {
  if (!turn?.resultID || !sessionID || socket?.readyState !== WebSocket.OPEN) return;
  socket.send(JSON.stringify({ type, session_id: sessionID, turn_id: turn.turnID,
    request_id: turn.requestID, generation: turn.generation, result_id: turn.resultID }));
}

async function stop({ abortPlayback = true } = {}) {
  epoch++;
  active = false;
  gate();
  if (abortPlayback) {
    try { playbackControl("abort"); } catch {}
  }
  pending = null;
  sessionID = "";
  generation = 0;
  try { socket?.send(JSON.stringify({ type: "stop" })); } catch {}
  socket?.close();
  socket = null;
  playback?.stop();
  playback = null;
  media?.getTracks().forEach(track => track.stop());
  media = null;
  await audioContext?.close();
  audioContext = null;
  startButton.disabled = false;
  stopButton.disabled = true;
  setStatus("停止中。再開するには Start を押してください。");
}

function matchesPlayback(message) {
  return active && sessionID === message.session_id && pending?.turnID === message.turn_id &&
    pending?.requestID === message.request_id && pending?.generation === message.generation &&
    pending?.resultID === message.result_id;
}

function failPlayback(message) {
  if (!matchesPlayback(message)) return;
  try { playbackControl("playback_failed"); } catch {}
  setStatus("再生できません。接続を終了します。");
  stop({ abortPlayback: false });
}

function onEvent(message) {
  if (!active || message.session_id !== sessionID && sessionID) return;
  if (message.type === "ready") {
    if (!sessionID) sessionID = message.session_id;
    if (message.generation <= generation) return;
    generation = message.generation;
    pending = null;
    if (!document.hidden) {
      listening = true;
      capture.port.postMessage({ type: "resume", generation });
      setStatus("聞き取り中");
    }
    return;
  }
  if (message.type === "processing") {
    if (message.generation !== generation || pending) return;
    gate();
    pending = { turnID: message.turn_id, requestID: message.request_id, generation };
    setStatus("文字起こしと返答を待っています");
    return;
  }
  if (message.type === "transcript") {
    if (pending && message.turn_id === pending.turnID && message.request_id === pending.requestID &&
        message.generation === pending.generation) appendLine("発話", message.text);
    return;
  }
  if (message.type === "error") {
    gate();
    setStatus(`処理できませんでした: ${message.error}`);
    return;
  }
  if (message.type !== "reply" || !pending) return;
  if (message.turn_id !== pending.turnID || message.request_id !== pending.requestID ||
      message.generation !== pending.generation || !message.result_id) return;
  if (pending.resultID) return;
  pending.resultID = message.result_id;
  gate();
  appendLine("返信", message.text);
  setStatus("返信を再生中");
  let audio;
  try {
    const raw = atob(message.audio);
    audio = Uint8Array.from(raw, character => character.charCodeAt(0));
  } catch {
    failPlayback(message);
    return;
  }
  let decoded;
  try { decoded = audioContext.decodeAudioData(audio.buffer); }
  catch { failPlayback(message); return; }
  decoded.then(buffer => {
    if (!matchesPlayback(message)) return;
    const source = audioContext.createBufferSource();
    source.buffer = buffer;
    source.connect(audioContext.destination);
    playback = source;
    source.onended = () => {
      if (!matchesPlayback(message)) return;
      playback = null;
      setTimeout(() => {
        if (!matchesPlayback(message)) return;
        try { playbackControl("played"); } catch { stop(); return; }
        setStatus("次の聞き取りを準備中");
      }, 350);
    };
    try { source.start(); } catch { failPlayback(message); }
  }).catch(() => {
    failPlayback(message);
  });
}

async function start() {
  if (active) return;
  const thisEpoch = ++epoch;
  const token = tokenInput.value;
  if (!/^[A-Za-z0-9_-]{32,}$/.test(token)) {
    setStatus("接続キーを確認してください。");
    return;
  }
  active = true;
  startButton.disabled = true;
  stopButton.disabled = false;
  try {
    const context = new AudioContext();
    audioContext = context;
    await context.resume();
    if (thisEpoch !== epoch) { await context.close(); return; }
    await context.audioWorklet.addModule("/worklet.js");
    if (thisEpoch !== epoch) { await context.close(); return; }
    const stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
    if (thisEpoch !== epoch) { stream.getTracks().forEach(track => track.stop()); return; }
    media = stream;
    const source = audioContext.createMediaStreamSource(media);
    capture = new AudioWorkletNode(audioContext, "capture-processor");
    const silence = audioContext.createGain();
    silence.gain.value = 0;
    source.connect(capture).connect(silence).connect(audioContext.destination);
    capture.port.onmessage = ({ data }) => {
      if (data.type === "overflow" && thisEpoch === epoch && listening && data.generation === generation) {
        setStatus("録音が追いつきません。Stop して言い直してください。");
        stop();
        return;
      }
      if (thisEpoch !== epoch || !active || !listening || data.generation !== generation || socket?.readyState !== WebSocket.OPEN) return;
      if (socket.bufferedAmount > 64 * 1024) {
        setStatus("送信が遅れています。Stop して再開してください。");
        stop();
        return;
      }
      const packet = new ArrayBuffer(4 + data.audio.byteLength);
      new DataView(packet).setUint32(0, generation, true);
      new Uint8Array(packet, 4).set(new Uint8Array(data.audio));
      socket.send(packet);
      capture.port.postMessage({ type: "credit", generation });
    };
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    socket = new WebSocket(`${scheme}//${location.host}/stream`, ["voice-v1", `auth.${token}`]);
    socket.onmessage = ({ data }) => {
      if (thisEpoch !== epoch) return;
      try { onEvent(JSON.parse(data)); } catch { setStatus("応答を読めませんでした"); }
    };
    socket.onclose = () => { if (thisEpoch === epoch && active) stop(); };
    socket.onerror = () => { if (thisEpoch === epoch && active) setStatus("接続できませんでした"); };
    setStatus("接続中");
  } catch (error) {
    if (thisEpoch !== epoch) return;
    setStatus(`開始できませんでした: ${error.message}`);
    await stop();
  }
}

startButton.addEventListener("click", start);
stopButton.addEventListener("click", stop);
document.addEventListener("visibilitychange", () => {
  if (!active) return;
  if (document.hidden) stop();
});
