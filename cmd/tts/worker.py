"""tts 的显卡进程：Go 那边按需拉起，一个进程只装一种模型。

    python worker.py <模型目录> <custom|clone|design>

stdin 一行一个 JSON 请求，stdout 一行一个 JSON 回复。装模型时先发
{"event": "loading"}，装好发 {"event": "ready"}，之后每个请求回
{"id": .., "ok": true, "secs": 音频秒数, "took": 用时} 或 {"id": .., "ok": false, "error": ..}。
stdin 一关就退出——Go 那边闲久了就是这么把显存放掉的。

这个文件是 tts 二进制里嵌着的，每次启动写到数据目录，改这里要重新 go install。
"""

import json
import os
import sys
import time
import traceback

# 协议走原来的 stdout；库里随手 print 的东西全改道到 stderr，免得把协议搅乱
_proto = os.fdopen(os.dup(1), "w", buffering=1, encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr

MODELS = {
    "custom": "Qwen3-TTS-12Hz-1.7B-CustomVoice",  # 9 个预设音色，能听「语气」指令
    "clone": "Qwen3-TTS-12Hz-1.7B-Base",  # 照一段录音克隆
    "design": "Qwen3-TTS-12Hz-1.7B-VoiceDesign",  # 用一句话描述出一个声音
}


def send(obj):
    _proto.write(json.dumps(obj, ensure_ascii=False) + "\n")
    _proto.flush()


def load(models_dir, kind):
    path = os.path.join(models_dir, MODELS[kind])
    if not os.path.exists(os.path.join(path, "model.safetensors")):
        raise RuntimeError(f"模型还没下好：{path}")

    import torch

    if not torch.cuda.is_available():
        raise RuntimeError("找不到 N 卡：独显是不是关了（dgpu on 打开）")

    from faster_qwen3_tts import FasterQwen3TTS

    model = FasterQwen3TTS.from_pretrained(path)
    model.warmup()  # 先把 CUDA graph 抓好，第一句就不用等
    return model


def generate(model, kind, req):
    text = req["text"]
    lang = req.get("language") or "auto"
    if kind == "custom":
        wavs, sr = model.generate_custom_voice(
            text=text,
            speaker=req["speaker"],
            language=lang,
            instruct=req.get("instruct") or None,
        )
    elif kind == "clone":
        ref_text = req.get("ref_text") or ""
        # 有参考录音的原文就走 ICL（像得多），没有就只用声纹
        wavs, sr = model.generate_voice_clone(
            text=text,
            language=lang,
            ref_audio=req["ref_audio"],
            ref_text=ref_text,
            xvec_only=not ref_text,
        )
    else:
        wavs, sr = model.generate_voice_design(text=text, instruct=req["instruct"], language=lang)
    return wavs, sr


def main():
    models_dir, kind = sys.argv[1], sys.argv[2]
    send({"event": "loading", "kind": kind})
    t0 = time.time()
    try:
        model = load(models_dir, kind)
    except Exception as e:
        traceback.print_exc()
        send({"event": "failed", "error": explain(e)})
        return
    send({"event": "ready", "kind": kind, "took": round(time.time() - t0, 1)})

    import numpy as np
    import soundfile as sf

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        req = json.loads(line)
        t0 = time.time()
        try:
            wavs, sr = generate(model, kind, req)
            audio = np.concatenate([np.asarray(w, dtype=np.float32).reshape(-1) for w in wavs])
            sf.write(req["out"], audio, sr, subtype="PCM_16")
            send({"id": req["id"], "ok": True, "secs": round(len(audio) / sr, 2), "took": round(time.time() - t0, 2)})
        except Exception as e:
            traceback.print_exc()
            send({"id": req["id"], "ok": False, "error": explain(e)})


def explain(e):
    msg = f"{type(e).__name__}: {e}"
    if "out of memory" in msg.lower():
        return "显存不够（是不是别的程序也在用显卡？）：" + msg
    return msg


if __name__ == "__main__":
    main()
