# 宿主 UI 接线：OCR（rapidocr-onnx → 转换图 img→txt）

## Context

rapidocr-onnx 内核能力已实现并通过 E2E（`registry.call rapidocr-onnx.ocr` 识别 "2026\nHello OctPlugins"，754ms，registry.json 已登记 rapidocr/rapidocr-rec 两条模型）。但宿主 UI 尚无 OCR 入口：用户无法在界面里"选图片 → 识别 → 看文本"。

conversion.md §三 3.3 明确 OCR 是转换图中的一个 hop：`img → txt`（RapidOCR）。转换图架构是"边由 tool manifest 的 `conversions` 声明，conversion 插件启动时 `scanManifests`/`buildEdges` 汇总，`reachable`/`convert` 自动覆盖"（[graph.go](file:///D:/Project/ElectronProject/OctPlugins/plugins/conversion/graph.go)）。因此宿主 UI 接线 = 让 rapidocr-onnx 声明 `img→txt` 转换边 + 后端兼容 conversion 的 `{input,output}` 调用契约 + 重建/重启 + UI 展示识别文本。**宿主 main.ts / renderer.ts 零改动**（文件关联注入、conversion 面板矩阵/执行均已就绪）。

## 改动

### 1. rapidocr-onnx 后端兼容 conversion 契约（[main.go](file:///D:/Project/ElectronProject/OctPlugins/plugins/rapidocr-onnx/main.go)）

`handleOcr` 增加 conversion 模式：参数 `{input, output, from, to}` 与现有 `{image}` 双兼容。
- `Image` 为空时用 `Input` 作为图片路径。
- `Output` 非空时把识别文本写入该文件（UTF-8，`text+"\n"`），返回体附加 `output` 字段；conversion `handleConvert` 以 `os.Stat(out)` 判成功（[convert.go](file:///D:/Project/ElectronProject/OctPlugins/plugins/conversion/convert.go#L116-L119)）。
- 保持原返回 `{ok, text, lines, engine, backend, ms}` 不变，不影响现有调用方。

### 2. rapidocr-onnx manifest 声明转换边（[manifest.json](file:///D:/Project/ElectronProject/OctPlugins/plugins/rapidocr-onnx/manifest.json)）

新增 `conversions`（对齐 media-convert 声明格式）：

```json
"conversions": [
  { "from": ["png", "jpg", "jpeg", "bmp", "gif"], "to": ["txt"], "method": "ocr",
    "note": "OCR 识别（PP-OCRv6_tiny，CPU EP）；输出 UTF-8 文本" }
]
```

- `resolveFn` 自动命中 functions 中 method=ocr 的 `rapidocr-onnx.ocr`（[graph.go L122-L129](file:///D:/Project/ElectronProject/OctPlugins/plugins/conversion/graph.go#L122-L129)），Fn 无需改。
- jpeg 归一为 jpg（`fmtAlias`）。

### 3. 构建与重基线

- `build.bat` 重建 rapidocr-onnx.exe。
- **registry.json 重基线**：删除 `state/registry.json` 中 `rapidocr-onnx` 条目（manifest 已变，保留旧 manifestHash 会触发 tampered 拒绝启动，项目记忆踩坑2）；内核 boot 时自动重新登记新 manifestHash。
- 重启内核，conversion 插件启动时 `buildEdges` 重新扫 rapidocr-onnx manifest（conversion 为 lazy 插件，首次 `conversion.*` 调用时启动建图，无需重建 conversion.exe）。

### 4.（增强）conversion 透传 tool 结果 + UI 文本预览

- [convert.go](file:///D:/Project/ElectronProject/OctPlugins/plugins/conversion/convert.go) `handleConvert`：循环内保留最后一次 `s.callFunc` 返回的 `json.RawMessage`，成功路径并入响应 `"toolResult": <raw>`（`callFunc` 已解包 `{ok,result}` 返回 tool 的 Result）。
- [app.ts](file:///D:/Project/ElectronProject/OctPlugins/plugins/conversion/ui/src/app.ts) `doConvert`：`r.toolResult && r.toolResult.text` 时在结果区渲染 `<pre>` 文本预览（不读文件，直接用 rapidocr 返回的 text）。
- 重编译 conversion.exe（`build.bat`）+ tsc 编译 ui/app.js（`node ..\..\..\ui\node_modules\typescript\bin\tsc -p ui\tsconfig.json`）。

## 验证（E2E，复用 mvp_test 模式）

1. 删除 registry 条目 → spawn kerneld → kernel.hello → `registry.list` 确认 rapidocr-onnx 已重登记。
2. `registry.call conversion.convert {input: mvp_test/ocr_test.png, from:"png", to:"txt", output:<临时 .txt>}` → 断言输出文件存在且内容含 "Hello OctPlugins"，响应 `toolResult.text` 非空。
3. `registry.call conversion.reachable {from:"png"}` → 断言 targets 含 `txt`。
4. UI 冒烟：conversion 面板打开后格式矩阵 PNG 可达含 TXT；打开图片文件（file:open 注入）→ applyInputPath 列可达；执行转换 → 结果区显示识别文本。

## 未完成缺口（如实声明）

- 无 cls 模型：竖排/旋转 90° 文字识别弱（已有）。
- 输入仅 png/jpg/jpeg/bmp/gif（webp/avif 需额外解码器）。
- 宿主"打开方式"对 png/jpg 等扩展的注册若被 image-convert 已覆盖则复用现有注册，不重复新增（faScanExtensions 行为以实测为准）。
