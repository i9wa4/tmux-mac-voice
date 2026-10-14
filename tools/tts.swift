import AVFoundation
import Foundation

// AVSpeechSynthesizer delivers generated PCM through an in-memory callback.
// This helper writes a bounded 16-bit mono WAV to stdout and no audio file.
let textData = FileHandle.standardInput.readDataToEndOfFile()
guard textData.count > 0, textData.count <= 8192,
      let text = String(data: textData, encoding: .utf8),
      let voice = AVSpeechSynthesisVoice(language: "ja-JP") else {
    fputs("invalid text or Japanese voice unavailable\n", stderr)
    exit(1)
}

let utterance = AVSpeechUtterance(string: text)
utterance.voice = voice
let synthesizer = AVSpeechSynthesizer()
let lock = NSLock()
var samples = Data()
var sampleRate: UInt32 = 0
var failed = false
var completed = false

synthesizer.write(utterance) { buffer in
    guard let pcm = buffer as? AVAudioPCMBuffer else {
        lock.lock(); failed = true; completed = true; lock.unlock()
        return
    }
    if pcm.frameLength == 0 {
        lock.lock(); completed = true; lock.unlock()
        return
    }
    guard let channels = pcm.floatChannelData, pcm.format.channelCount >= 1 else {
        lock.lock(); failed = true; completed = true; lock.unlock()
        return
    }
    lock.lock()
    defer { lock.unlock() }
    let rate = UInt32(pcm.format.sampleRate.rounded())
    if sampleRate != 0 && sampleRate != rate { failed = true; return }
    sampleRate = rate
    if samples.count + Int(pcm.frameLength) * 2 > 4 * 1024 * 1024 {
        failed = true
        return
    }
    for i in 0..<Int(pcm.frameLength) {
        let value = max(-1.0, min(1.0, channels[0][i]))
        var encoded = Int16((value * 32767).rounded()).littleEndian
        withUnsafeBytes(of: &encoded) { samples.append(contentsOf: $0) }
    }
}

let deadline = Date(timeIntervalSinceNow: 30)
while Date() < deadline {
    lock.lock()
    let done = completed
    lock.unlock()
    if done { break }
    RunLoop.current.run(mode: .default, before: Date(timeIntervalSinceNow: 0.05))
}
lock.lock()
let done = completed
lock.unlock()
if !done {
    fputs("speech synthesis timed out\n", stderr)
    exit(1)
}
lock.lock()
let valid = !failed && sampleRate > 0 && !samples.isEmpty
lock.unlock()
guard valid else {
    fputs("speech synthesis failed\n", stderr)
    exit(1)
}

func appendLE<T: FixedWidthInteger>(_ value: T, to data: inout Data) {
    var little = value.littleEndian
    withUnsafeBytes(of: &little) { data.append(contentsOf: $0) }
}
var wav = Data()
wav.append("RIFF".data(using: .ascii)!)
appendLE(UInt32(36 + samples.count), to: &wav)
wav.append("WAVEfmt ".data(using: .ascii)!)
appendLE(UInt32(16), to: &wav)
appendLE(UInt16(1), to: &wav)
appendLE(UInt16(1), to: &wav)
appendLE(sampleRate, to: &wav)
appendLE(sampleRate * 2, to: &wav)
appendLE(UInt16(2), to: &wav)
appendLE(UInt16(16), to: &wav)
wav.append("data".data(using: .ascii)!)
appendLE(UInt32(samples.count), to: &wav)
wav.append(samples)
FileHandle.standardOutput.write(wav)
