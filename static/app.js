"use strict";

const screen = document.getElementById("screen");
const status = document.getElementById("status");
const connectButton = document.getElementById("connect");
const disconnectButton = document.getElementById("disconnect");
const input = document.getElementById("keyboard-input");
let client = null;
const keyboard = new Guacamole.Keyboard(input);
const forwardedKeys = new Map();
let connected = false;
let cancelPaste = null;
let authenticated = false;

function resetKeyboard() {
  keyboard.reset();
  for (const keysym of forwardedKeys.values()) client?.sendKeyEvent(0, keysym);
  forwardedKeys.clear();
  input.value = "";
  composed = [];
}

function focusRemote() {
  if (connected) input.focus({ preventScroll: true });
}

keyboard.onkeydown = (keysym) => {
  if (!connected) return true;
  // 브라우저가 로컬 클립보드 내용으로 paste 이벤트를 보내도록 그대로 둔다.
  if (
    ((keyboard.modifiers.ctrl || keyboard.modifiers.meta) &&
      (keysym === 0x0076 || keysym === 0x0056)) ||
    (keyboard.modifiers.shift && keysym === 0xff63)
  )
    return true;
  // macOS Command 단축키를 Windows Ctrl 단축키로 바꾼다.
  const remoteKey = keysym === 0xffe7 || keysym === 0xffe8 ? 0xffe3 : keysym;
  forwardedKeys.set(keysym, remoteKey);
  client.sendKeyEvent(1, remoteKey);
  return false;
};
keyboard.onkeyup = (keysym) => {
  const remoteKey = forwardedKeys.get(keysym);
  if (remoteKey !== undefined) client?.sendKeyEvent(0, remoteKey);
  forwardedKeys.delete(keysym);
};

for (const type of ["keydown", "keyup"]) {
  window.addEventListener(
    type,
    (event) => {
      if (
        event.target === input &&
        (event.ctrlKey || event.metaKey) &&
        /^Key[A-Z]$/.test(event.code) &&
        !/^[ -~]$/.test(event.key)
      ) {
        const letter = event.code.slice(3);
        Object.defineProperty(event, "key", {
          value: event.shiftKey ? letter : letter.toLowerCase(),
        });
      }
    },
    true,
  );
}

let composed = [];
function syncComposition() {
  if (!connected) return;
  const next = Array.from(input.value);
  let same = 0;
  while (same < composed.length && composed[same] === next[same]) same++;
  for (let i = same; i < composed.length; i++) {
    keyboard.press(0xff08);
    keyboard.release(0xff08);
  }
  keyboard.type(next.slice(same).join(""));
  composed = next;
}
// Guacamole이 같은 글자를 한 번 더 입력하지 않도록 input과 compositionend는 여기서만 처리한다.
for (const type of ["input", "compositionend"]) {
  window.addEventListener(
    type,
    (event) => {
      if (event.target !== input) return;
      event.stopPropagation();
      syncComposition();
    },
    true,
  );
}
// 입력기를 거치지 않는 키(엔터, 방향키, 영문)는 그대로 전송되고 원격 커서를 옮길 수 있으므로 입력칸 기준을 새로 시작한다.
window.addEventListener(
  "keydown",
  (event) => {
    if (event.target === input && !event.isComposing && event.keyCode !== 229) {
      input.value = "";
      composed = [];
    }
  },
  true,
);
input.addEventListener("blur", resetKeyboard);
input.addEventListener("paste", (event) => {
  if (!connected) return;
  event.preventDefault();
  const contents = event.clipboardData?.getData("text/plain");
  if (contents) pasteText(contents);
});
screen.onpointerdown = focusRemote;
screen.onfocus = focusRemote;
window.addEventListener("blur", () => {
  resetKeyboard();
  if (cancelPaste) cancelPaste();
});

async function authenticate() {
  const token = document.getElementById("token").value;
  const response = await fetch("/session", {
    method: "POST",
    headers: { "X-Winboat-Token": token || "" },
  });
  if (!token && !response.ok) return;
  if (!response.ok)
    throw new Error(
      "접속 주소가 만료되었습니다. 서버에 표시된 주소를 사용하세요.",
    );
  authenticated = true;
}

function stop() {
  resetKeyboard();
  if (cancelPaste) cancelPaste();
  connected = false;
  input.readOnly = true;
  const session = client;
  client = null;
  if (session) session.disconnect();
  screen.hidden = true;
  connectButton.disabled = false;
  disconnectButton.disabled = true;
}

function connect() {
  stop();
  status.textContent = "Windows에 연결하는 중…";
  screen.replaceChildren();
  screen.hidden = false;
  const tunnel = new Guacamole.WebSocketTunnel(`ws://${location.host}/tunnel`);
  const session = (client = new Guacamole.Client(tunnel));
  const display = session.getDisplay();
  screen.appendChild(display.getElement());
  session.onerror = (error) => {
    if (client !== session) return;
    stop();
    status.textContent =
      error.message || "연결에 실패했습니다. 서버에 표시된 주소를 사용하세요.";
  };
  session.onstatechange = (state) => {
    if (client !== session) return;
    if (state === Guacamole.Client.State.CONNECTED) {
      connected = true;
      input.readOnly = false;
      status.textContent = "연결됨 · 화면을 클릭해서 조작하세요";
      disconnectButton.disabled = false;
      focusRemote();
    } else if (state === Guacamole.Client.State.DISCONNECTED) {
      stop();
      status.textContent = "연결 해제됨";
    }
  };
  const mouse = new Guacamole.Mouse(display.getElement());
  // 원격 커서를 따로 그리지 않고, 원격 커서 모양을 로컬 커서에 입힌다.
  display.oncursor = (canvas, x, y) =>
    display.showCursor(!mouse.setCursor(canvas, x, y));
  mouse.onEach(["mousedown", "mouseup", "mousemove"], (event) => {
    if (client === session && connected)
      session.sendMouseState(event.state, true);
  });
  session.onclipboard = (stream, mimetype) => {
    if (mimetype !== "text/plain") {
      stream.sendAck("Unsupported clipboard type", 0x0100);
      return;
    }
    const reader = new Guacamole.StringReader(stream);
    let contents = "";
    reader.ontext = (chunk) => {
      contents += chunk;
      stream.sendAck("OK", 0);
    };
    reader.onend = async () => {
      if (client !== session || !connected) return;
      try {
        await navigator.clipboard.writeText(contents);
      } catch {
        if (client === session && connected)
          status.textContent =
            "브라우저가 클립보드 쓰기를 막아 원격 텍스트를 복사하지 못했습니다.";
      }
    };
  };
  connectButton.disabled = true;
  const viewport = document.querySelector("main");
  session.connect(
    new URLSearchParams({
      width: Math.max(640, viewport.clientWidth),
      height: Math.max(480, viewport.clientHeight),
    }).toString(),
  );
}

connectButton.onclick = connect;
disconnectButton.onclick = () => {
  stop();
  status.textContent = "연결 해제됨";
};
function pasteText(contents) {
  if (!connected || !contents || cancelPaste) return;
  const session = client;
  resetKeyboard();
  const stream = session.createClipboardStream("text/plain");
  const writer = new Guacamole.StringWriter(stream);
  let timer = null;
  const finish = () => {
    if (cancelPaste !== finish) return;
    clearTimeout(timer);
    cancelPaste = null;
  };
  cancelPaste = finish;
  writer.onack = (ack) => {
    if (cancelPaste === finish && ack.isError()) {
      finish();
      if (client === session && connected)
        status.textContent = "텍스트 붙여넣기에 실패했습니다.";
    }
  };
  writer.sendText(contents);
  writer.sendEnd();
  timer = setTimeout(() => {
    if (client === session && connected) {
      resetKeyboard();
      session.sendKeyEvent(1, 0xffe3);
      session.sendKeyEvent(1, 0x0076);
      session.sendKeyEvent(0, 0x0076);
      session.sendKeyEvent(0, 0xffe3);
      focusRemote();
    }
    finish();
  }, 150);
  return true;
}

window.addEventListener("beforeunload", stop);
authenticate()
  .then(() => {
    if (authenticated) status.textContent = "연결 버튼을 누르세요";
    else status.textContent = "서버에 표시된 접속 주소를 열어 주세요";
  })
  .catch((error) => {
    status.textContent = error.message;
  });
