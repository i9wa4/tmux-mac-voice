class CaptureProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.enabled = false;
    this.phase = 0;
    this.sum = 0;
    this.count = 0;
    this.frame = new Int16Array(320);
    this.position = 0;
    this.generation = 0;
    this.credits = 0;
    this.port.onmessage = ({ data }) => {
      if (data?.type === "credit") {
        if (this.enabled && data.generation === this.generation) this.credits = Math.min(4, this.credits + 1);
        return;
      }
      this.enabled = data?.type === "resume";
      this.credits = this.enabled ? 4 : 0;
      if (this.enabled) this.generation = data.generation;
      if (!this.enabled) {
        this.position = 0;
        this.phase = 0;
        this.sum = 0;
        this.count = 0;
      }
    };
  }

  process(inputs) {
    const input = inputs[0]?.[0];
    if (!this.enabled || !input) return true;
    for (const value of input) {
      this.sum += value;
      this.count++;
      this.phase += 16000 / sampleRate;
      if (this.phase < 1) continue;
      this.phase -= 1;
      const averaged = Math.max(-1, Math.min(1, this.sum / this.count));
      this.frame[this.position++] = Math.round(averaged * 32767);
      this.sum = 0;
      this.count = 0;
      if (this.position === 320) {
        if (this.credits > 0) {
          this.port.postMessage({ generation: this.generation, audio: this.frame.buffer }, [this.frame.buffer]);
          this.credits--;
        } else {
          this.port.postMessage({ type: "overflow", generation: this.generation });
          this.enabled = false;
        }
        this.frame = new Int16Array(320);
        this.position = 0;
      }
    }
    return true;
  }
}

registerProcessor("capture-processor", CaptureProcessor);
