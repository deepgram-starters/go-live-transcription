let isRecording = false;
let websocket;
let microphone;

async function getMicrophone() {
  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    return new MediaRecorder(stream, { mimeType: "audio/webm" });
  } catch (error) {
    console.error("error accessing microphone:", error);
    throw error;
  }
}

async function openMicrophone(microphone, socket) {
  return new Promise((resolve) => {
    microphone.onstart = () => {
      console.log("client: microphone opened");
      document.body.classList.add("recording");
      resolve();
    };
    microphone.ondataavailable = async (event) => {
      console.log("client: microphone data received");
      if (event.data.size > 0 && socket.readyState === WebSocket.OPEN) {
        socket.send(event.data);
      }
    };
    microphone.start(1000);
  });
}

async function startRecording() {
  isRecording = true;
  microphone = await getMicrophone();
  console.log("client: waiting to open microphone");
  websocket = new WebSocket("ws://localhost:8080/ws");
  console.log("client: connected to server");
  await openMicrophone(microphone, websocket);
  document.body.classList.add("recording");
  websocket.onmessage = (event) => {
    document.getElementById("captions").innerHTML = JSON.parse(event.data);
  };
}

async function stopRecording() {
  if (isRecording === true) {
    microphone.stop();
    websocket.send(JSON.stringify({ type: "closeMicrophone" }));
    microphone = null;
    isRecording = false;
    console.log("client: microphone closed");
    if (websocket) {
      websocket.close();
      websocket = null;
      console.log("client: disconnected from server");
    }
    document.body.classList.remove("recording");
  }
}

document.addEventListener("DOMContentLoaded", () => {
  const recordButton = document.getElementById("record");

  recordButton.addEventListener("click", () => {
    if (!isRecording) {
      startRecording();
    } else {
      stopRecording();
    }
  });
});
