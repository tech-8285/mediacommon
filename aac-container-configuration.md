# AAC 音频在 ADTS 与 LATM 之间转封装的配置可表达范围分析

> 分析对象：本仓库 `pkg/codecs/mpeg4audio` 与 `pkg/formats/mpegts`、`internal/mp4` 中的实际实现（分支 `task/49`，HEAD `059ab2e`）。
> 方法：逐条阅读源码与现有测试，并在隔离的临时模块中以位级构造的输入实际运行解析/封装修复路径进行验证（本环境 Go 工具链为 1.20，无法直接构建要求 go 1.26 的仓库；实验仅复制相关包并做语法降级，业务代码未改）。
> 证据分级约定：**【源码】** 表示由代码路径直接决定；**【测试】** 表示仓库已有测试材料覆盖；**【实验】** 表示本次用构造输入实际运行得到的结果。报告不包含任何真实解码器输出，凡涉及“能否正确出声/解释”的结论均严格限定在源码能证明的范围内。

---

## 1. 两种封装的配置结构与当前实现能提取的音频属性

### 1.1 涉及的结构与解析入口

| 封装 | 配置载体 | 结构/入口 | 关键源码 |
|---|---|---|---|
| ADTS | 每帧 7 字节（或带 CRC 的 9 字节）固定头 | `ADTSPacket` / `ADTSPackets.Unmarshal`、`Marshal` | `pkg/codecs/mpeg4audio/adts.go` |
| LATM | `AudioSyncStream`（syncword `0x2B7`）内的 `AudioMuxElement`，其 `StreamMuxConfig` 内嵌一个或多个 `AudioSpecificConfig` | `AudioSyncStream`、`AudioMuxElement`、`StreamMuxConfig`、`AudioSpecificConfig` | `audio_sync_stream.go`、`audio_mux_element.go`、`stream_mux_config.go`、`audio_specific_config.go` |
| MP4/fMP4（对照） | esds 框中的 DecoderSpecificInfo，即裸 `AudioSpecificConfig` | `internal/mp4/codec_boxes.go` | 读：`codec_boxes.go:477`；写：`codec_boxes.go:1091-1124` |

`AudioSpecificConfig`（下称 **ASC**）是 LATM 与 MP4 的共同配置语言；ADTS 不携带 ASC，mpegts 层在探测阶段手工把 ADTS 头字段拼成一个 ASC（`pkg/formats/mpegts/track.go:62-96` 的 `findMPEG4AudioConfig`）。

ASC 当前能表达并被实现解析的字段（`audio_specific_config.go:16-37`）：

- `Type`（核心编码对象类型，只接受 `AACLC=2`、`SBR=5`、`PS=29`，见 `audio_specific_config.go:65-69` 与 `object_type.go`）
- `SampleRate`（核心编码采样率，支持索引 0–12 与转义索引 `0xF` + 24 位显式频率，`audio_specific_config.go:76-89`）
- `ChannelConfig` / `ChannelCount`（声道配置，见 1.3）
- `ExtensionType`、`ExtensionSampleRate`（仅当外层对象类型为 SBR/PS 时出现的扩展部分，`audio_specific_config.go:120-153`）
- `FrameLengthFlag`、`DependsOnCoreCoder`、`CoreCoderDelay`（GASpecificConfig，`audio_specific_config.go:157-173`）

### 1.2 核心采样率与扩展输出采样率并存（HE-AAC v1/v2 的显式向后兼容配置）

当 ASC 外层对象类型为 SBR（5）或 PS（29）时，比特流按“扩展对象类型 + 核心采样率 + 声道 + 扩展采样率 + 核心对象类型”的顺序编码（ISO 14496-3 的 explicit-backwards-compatible 形式）。实现对此的处理（`audio_specific_config.go:120-153`）：

1. 把外层类型先记入 `ExtensionType`；
2. 读取扩展采样率索引（同样支持 `0xF`+24 位显式值）；
3. 再读 5 位核心对象类型，并要求它**必须是 AAC-LC（2）**，否则报 `unsupported object type`。

因此，双采样率并存时当前实现**可以同时得到**以下属性：

- 核心编码采样率 `SampleRate`（SBR/PS 核心 AAC-LC 实际工作的低采样率）；
- 扩展输出采样率 `ExtensionSampleRate`（SBR 重建后的高采样率）；
- `ExtensionType`（5=SBR/HE-AAC v1，29=PS/HE-AAC v2）；
- 核心对象类型被规范化为 `Type=AACLC`；
- 声道配置（`ChannelConfig`/`ChannelCount`）与 GASpecificConfig 三个字段。

**【测试】** 仓库测试同时覆盖了这两种情形：`audio_specific_config_test.go` 的 `"sbr (he-aac v1) 44.1khz mono"`（核心 22050 + 扩展 44100）、`"sbr (he-aac v1) 44.1khz stereo"`、`"ps (he-aac v2) 48khz stereo"`（核心 24000 + 扩展 48000）；LATM 侧 `stream_mux_config_test.go` 的 `"sbr"`、`"ps"` 用例验证了嵌在 `StreamMuxConfig` 内的同一结构。
**【实验】** 构造 SBR + `channelConfig=0` 的 ASC（字节 `2c 02 08 00`），解析得到 `Type=2, SampleRate=16000, ExtensionType=5, ExtensionSampleRate=44100, ChannelConfig=0`，且重新 `Marshal` 后与原字节完全一致——双采样率与 PCE 声道标记可以在 ASC/LATM 中同时保留。

需要强调的表达边界（【源码】）：

- 扩展部分**只识别 SBR/PS 两种外层类型**，且扩展后的核心类型只接受 LC；其它对象类型在 ASC 第一层即被拒绝（`audio_specific_config.go:65-69`，【实验】对象类型 3（AAC-SSR）报 `unsupported object type: 3`）。
- 采样率索引 13、14 被显式拒绝（`audio_specific_config.go:87-88`，【实验】分别报 `invalid sample rate index (13/14)`）；任何不在 13 个表项内的频率必须走 `0xF` 转义。
- GASpecificConfig 的 `extensionFlag` 必须为 0，否则报 `extensionFlag is unsupported`（`audio_specific_config.go:180-182`，【实验】构造该位置 1 的输入确认报错）。

### 1.3 声道布局由节目配置元素（PCE）给出（channelConfig = 0）

ADTS 头与 ASC 都只有一个 4 位 `channel_configuration` 字段。实现对取值的处理两处一致（`adts.go:67-90`、`audio_specific_config.go:97-118`）：

| channelConfig | 含义 | 实现行为 |
|---|---|---|
| 0 | 声道布局由 PCE 决定，PCE 可位于 GASpecificConfig 内或每个 AU 的 `raw_data_block` 起始 | **保留原值 0，`ChannelCount` 置 0**，不报错 |
| 1–6 | 声道数即配置值 | `ChannelCount = ChannelConfig` |
| 7 | 8 声道 | `ChannelCount = 8` |
| 8–15 | 保留值 | 解析报错（ADTS：`unsupported channel configuration`；ASC：`unsupported channel configuration`） |

`channelConfig=0` 时，实现本身**不从配置中得到任何声道数/布局信息**，只留下“布局在别处定义”的标记。真正的声道含义需要对 AU 调用两个导出辅助函数（二者在生产代码中**没有任何调用方**，仅供库使用者按需调用，【源码】全仓 grep 仅命中定义与注释）：

- `ParsePCEFromRawDataBlock(au)`（`program_config_element.go:185`）：要求 AU 第一个语法元素就是 PCE（`id_syn_ele=5`），解析前/侧/后/LFE 元素数量及每个元素的 `is_cpe`（单声道元素=1 声道，声道对元素=2 声道），汇总出 `ChannelCount`，并保留 PCE 的元素构成字段（`NumFrontChannelElements` 等）。**【测试】** `program_config_element_test.go` 覆盖立体声 PCE（1 个前向 CPE=2 声道）、带 mono/stereo mixdown 的 PCE、5.1 PCE（2 前向 + 1 后向 CPE + 1 LFE=6 声道）。
- `CountChannelsFromRawDataBlock(au)`（`count_channels.go:31`）：处理流中没有显式 PCE、靠元素顺序隐式推断的情形（注释列举了 CPE=立体声、SCE+CPE=3.0、SCE+CPE+CPE+LFE=5.1 等常见隐式布局）。

**这两个辅助函数的能力边界（均为源码可证，且影响“实际声道含义”的可得性）：**

1. `CountChannelsFromRawDataBlock` 在遇到**第一个** SCE/CPE/LFE 元素后立即 `return`（`count_channels.go:59、69、79`），不会继续统计后续元素。**【实验】** 构造 SCE+CPE+LFE 的 4 声道 AU，函数只返回 1 而不是 4。也就是说，代码注释所宣称的 3.0/5.1 等多元素隐式布局在当前实现中**算不出正确总数**；只有“单个元素即全部声道”的布局（单 SCE=1、单 CPE=2、单 LFE=1）才被测试证明正确（**【测试】** `count_channels_test.go` 全部用例均为单声道元素布局，外加 FIL/DSE 前缀跳过）。
2. `ParsePCEFromRawDataBlock` 只在 AU 以 PCE 开头时成功；以普通声道元素开头时返回 `expected PCE (id=5), got id=...`（`program_config_element.go:200-202`，【实验】对 CPE 开头的 AU 确认报错）。其 PCE 解析保留 `ElementInstanceTag`、`ObjectType`、`SamplingFrequencyIndex`、各类元素计数与汇总 `ChannelCount`，但 mono/stereo/matrix mixdown 编号只读后丢弃，且在数完声道后显式跳过 assoc_data、valid_cc、byte_alignment、comment 等剩余字段（`program_config_element.go:174-176`）；该结构只有 `unmarshal`、**没有 Marshal 能力**，无法把解析出的 PCE 重新写回任何配置。
3. 对位于 **GASpecificConfig 内部**的 PCE（ASC 中 `channelConfig=0` 的标准位置之一）：ASC 解析器读完三个 GASpecificConfig 标志位和 `extensionFlag` 后即结束（`audio_specific_config.go:155-184`），**完全不解析内嵌 PCE**。**【实验】** 构造在该位置带完整 PCE 的 ASC，解析成功但结构体里只有 `ChannelConfig=0/ChannelCount=0`，PCE 内容既不进结构体也无法再写出（`Marshal` 固定只写一个 0 的 `extensionFlag`，`audio_specific_config.go:289`）。更严格地，当该 PCE 的 `element_instance_tag` 高位恰好为 1 时，它会被误读成 `extensionFlag=1` 而整个配置被拒（**【实验】** 报 `extensionFlag is unsupported`）——即部分合法的“ASC 内嵌 PCE”流连头部解析都过不了。

### 1.4 配置复用机制与“缺失的新配置”如何影响后续解释

两个层次存在不同的“复用先前配置”机制：

**(a) LATM 元素级：`useSameStreamMux`（`audio_mux_element.go:31-45`）**

- `AudioMuxElement` 开头 1 位 `useSameStreamMux=1` 时元素内不含 `StreamMuxConfig`，必须由调用方从外部预先塞入 `e.StreamMuxConfig`。若没有任何可用配置，解析在 `audio_mux_element.go:49-51` 直接失败：`no StreamMuxConfig available`。**【实验】** 用仓库测试中的 `useSameStreamMux` 字节（`audio_mux_element_test.go`）在不提供外部配置的情况下解析，确认该报错；同测试也示范了正确用法——调用方把已知配置赋给 `StreamMuxConfig` 后才能解出载荷。
- mpegts 探测侧 `findMPEG4AudioLATMConfig`（`track.go:98-128`）一直跳过 `useSameStreamMux=1` 的元素，直到遇到第一个自带新配置的元素才建轨；运行期 `OnDataMPEG4AudioLATM`（`reader.go:459-475`）只做 `AudioSyncStream` 分帧，把原始 element 字节透传给回调，**库本身不缓存配置、不替调用方补配置**。因此一旦流中出现配置变更（新元素携带 `useSameStreamMux=0` + 新 SMC），轨道上保存的仍是“第一个带配置的包”的 SMC（`mpeg4_audio_latm.go:8-9` 注释明确写明这一点），后续解释若依赖轨道配置就会用过时参数；而 `useSameStreamMux=1` 的元素脱离该缓存后无法独立解析。

**(b) LATM 层内：`useSameConfig`（`stream_mux_config.go:93-110`）**

- 除 (program 0, layer 0) 外，每个层先读 1 位 `useSameConfig`；为 1 时该层 `AudioSpecificConfig` 指针为 `nil`，语义上复用同 program 上一层的配置。**【实验】** 构造 2 层、第 2 层共享配置的 SMC，Marshal/Unmarshal 往返后第 2 层 ASC 指针确为 `nil`。注意复用关系只以“`nil` 指针”表达，结构体里不复制配置内容，消费方必须自行沿层向上回溯。

**(c) ADTS 侧没有配置复用机制**：每一帧都带完整固定头，不存在“缺配置”状态；但 mpegts 写侧 `WriteMPEG4Audio`（`writer.go:222-247`）对每组 AU 都用轨道上唯一一份 ASC 字段现场生成 ADTS 头，流中途的参数变化在这个 API 层面无处表达。

“缺失新配置”对后续音频解释的具体影响（源码可证的范围）：

- LATM 中缺少可用 SMC 时，失败发生在**载荷解析之前**（拿不到 program/layer 数量与 `frameLengthType`，`payloadLengthInfo` 无法工作），表现为整元素不可解析，而不是“按旧参数错误解释”。
- 配置陈旧但仍可解析时，载荷本身（`payloadMux` 里的原始 AU 字节）仍被原样切分输出，库不做解码，所以错误不会在本库内暴露；真正受影响的是下游解码器拿到的采样率/声道语义。这一推断链条中“下游会如何错”超出源码证明范围，本报告不作断言。
- ADTS→ASC 探测只取**第一个** ADTS 包（`track.go:88-94`），且只搬 `Type/SampleRate/ChannelConfig/ChannelCount` 四个字段；ADTS 头里本就不存在的 SBR/PS、GASpecificConfig 信息无从产生（详见第 2 节）。

---

## 2. 两种封装互转时编码信息的保留、拒绝与丢失

### 2.1 表达能力总览（按字段）

| 编码信息 | ADTS 固定头 | ASC / LATM StreamMuxConfig | 互转结论（源码可证） |
|---|---|---|---|
| 对象类型 | 2 位 profile，仅 1–4（Marshal 强制，`adts.go:140-143`） | 5 位，仅 LC/SBR/PS 可解析；SBR/PS 以扩展形式存在 | LATM/MP4 的 SBR(5)、PS(29) **无法写入 ADTS**（【实验】`ADTS only supports ObjectType 1-4`）；ADTS 的 profile 1–4 可构造 ASC，但 ASC 解析器只接受 LC=2（【实验】类型 3 被 ASC 拒），实际可双向通行的只有 LC |
| 核心采样率 | 4 位索引，仅表内 13 项；Marshal 走 `reverseSampleRates` 反查（`adts.go:145-148`） | 同 13 项 + `0xF` 转义 24 位任意频率 | 转义频率（如测试中的 53000，`audio_specific_config_test.go` `"aac-lc 53khz stereo"`）在 LATM/MP4 可表达，**写 ADTS 被拒**（【实验】`invalid sample rate: 53000`） |
| 扩展输出采样率（SBR/PS） | **无对应字段** | 有，`ExtensionSampleRate`，支持转义频率 | LATM→ADTS 时该信息无位置可放，必然丢失（【实验】见 2.2 M3）；ADTS 从不产生该字段 |
| 声道配置 1–6 / 7 | 有，4 位 | 有，4 位 | 双向保留（含 `ChannelCount` 派生值） |
| 声道配置 0（PCE 定义布局） | 有，原样保留 0（`adts.go:75-85`） | 有，原样保留 0（`audio_specific_config.go:104-113`） | 配置层双向可传 0，但**两边都不带声道数**；LATM SMC 可容纳（【实验】SMC ch0 往返成功），MP4/fMP4 写框直接拒绝（`codec_boxes.go:1104-1105`，`MPEG-4 audio channelConfig = 0 is not supported (yet)`） |
| 带内 PCE（AU 起始） | AU 原样透传，不解析 | AU 作为 payload 原样透传，不解析 | 只要 AU 字节不被改动，带内 PCE 在 ADTS↔LATM 间**随载荷保留**；但库不把它提升为配置，声道数需调用方自行用辅助函数获取 |
| ASC 内嵌 PCE（GASpecificConfig 内） | 不适用 | 解析器不读取、Marshal 不写出 | 经 ASC 往返即丢失；部分 tag 取值还会触发 `extensionFlag` 误判而被拒（1.3 节） |
| FrameLengthFlag / DependsOnCoreCoder / CoreCoderDelay | 无 | 有，完整往返 | **【测试】** `"aac-lc 96khz stereo delay"`（`audio_specific_config_test.go`）与【实验】M6 证明 LATM/MP4 内可保留；ADTS 路径不携带，转 ADTS 即丢 |
| CRC（protection_absent=0） | 可解析、跳过且不校验（`adts.go:43-50`） | 不适用 | 【测试】`casesADTSCRC` 可读；重新 Marshal 恒定写 `0xF1`（protection_absent=1），**CRC 必然丢失**（【实验】P 确认） |
| 一帧多个 raw_data_block | 拒绝：`frame count greater than 1 is not supported`（`adts.go:104-107`） | LATM 以 subframe/program/layer 多维载荷表达 | ADTS 侧不支持多块；LATM 的多 program/多层结构无 ADTS 对应物 |

### 2.2 两个方向的具体行为

**ADTS → LATM/ASC（库内实际存在的路径是 mpegts 探测 `findMPEG4AudioConfig`）**

- 可保留：对象类型（LC）、表内采样率、`channelConfig` 1–7 及其派生声道数、`channelConfig=0` 标记；AU 字节整体进入 LATM payload，带内 PCE/隐式元素随之保留。
- 被拒绝：ADTS 头自身的解析错误（syncword、采样率索引 >12、保留声道配置 8–15、FrameLen 非法、AU 超 `MaxAccessUnitSize=5KiB`、多 raw_data_block，见 `adts.go:59-107`）会让建轨直接失败。
- 失去表达：ADTS 头里没有 SBR/PS 标志位与扩展采样率，也没有 GASpecificConfig 三字段——合成出的 ASC 天然不含这些信息。HE-AAC 流若以 ADTS 传输，其 SBR/PS 数据只存在于 AU 内部的 fill 元素中，本库不解析 AU，因此配置层看不到 HE-AAC 身份。

**LATM/ASC → ADTS（库内无一键转封装；按 `WriteMPEG4Audio` 的字段用法推演并经实验验证）**

- 【实验】M3：把 SBR 的 ASC（核心 24000/扩展 48000）按库的写法填入 ADTS 头，Marshal **成功**，但产出的头只声明 LC + 24000 + 声道数，`ExtensionType=5` 与 48000 扩展采样率无字段可写——**头部解析仍成功，配置语义已降级**。
- 【实验】M4：PS（类型 29）直接 `ADTSPacket.Type=29` 封装修复被拒（`ADTS only supports ObjectType 1-4`）。
- 【实验】M5：转义采样率 53000 封装修复被拒（`invalid sample rate`）。
- 【实验】D3：`channelConfig=0/ChannelCount=0` 可以写出 ADTS 头并再次解析回 0，配置标记不丢，但声道数仍需从 AU 恢复。
- LATM 的多 program、多 layer、`useSameConfig` 共享、`NumSubFrames` 等结构在单流 ADTS 中没有对应表达；`StreamMuxConfig` 级的 `OtherDataPresent`/CRC、`frameLengthType` 也无处安放。

### 2.3 LATM 自身的解析限制（决定哪些 LATM 流根本进不了转换）

`StreamMuxConfig`/`AudioMuxElement` 解析器的硬性拒绝条件（【源码】，关键项经【实验】复核）：

- `audioMuxVersion=1`：拒绝（`stream_mux_config.go:62-65`）。
- `allStreamsSameTimeFraming=0`：拒绝（`stream_mux_config.go:67-70`）。
- `frameLengthType`：只有 0 在载荷长度信息里被实现；类型 1/3/4/5/6/7 可通过 SMC 解析（字段照存），但到 `AudioMuxElement` 解析载荷时，`payloadLengthInfo.payloadLength` 对非 0 类型统一报 `unsupported frameLengthType`（`payload_length_info.go:113-123`，【实验】H2 复现）；Marshal 侧对非 0 类型同样报错，且因 `marshalSize` 少算长度字节，类型 1 还会触发**越界 panic** 而不是干净错误（【实验】H 复现 `index out of range`，属实现缺陷）。
- `OtherDataPresent=true`：SMC 本身能解析（**【测试】** `stream_mux_config_test.go` 的 `"other data and checksum"` 等用例），但 `AudioMuxElement.Unmarshal` 在载荷之后显式拒绝（`audio_mux_element.go:71-73`，`OtherDataPresent is not supported (yet)`），Marshal 则直接假定其为 false（`audio_mux_element.go:118、169`）。
- 截断的 SMC 被宽容处理：读不到 `frameLengthType` 时按 `LatmBufferFullness=255` 提前返回（`stream_mux_config.go:114-120`，**【测试】** `TestStreamMuxConfigUnmarshalTruncated`）。
- ASC 层的所有拒绝条件（对象类型、采样率索引、声道配置 8–15、extensionFlag）同样作用于嵌在 SMC 内的每个 ASC。

### 2.4 兼顾“原配置形式”与“实际声道含义”的兼容条件

下面把“头部解析成功”与“后续音频可被正确解释”明确分开。前者只要求 Unmarshal/Marshal 不报错；后者还要求配置提供了解码所需的全部参数。本库不做音频解码，第二列的判断仅依据“配置结构体是否拿齐语义”与“恢复声道的辅助函数能否成功”，不代表任何实测解码结果。

| 情形 | 头部解析/封装修复成功的条件 | 后续音频可正确解释所需的额外条件（源码可证部分） |
|---|---|---|
| 普通 LC，channelConfig 1–7，表内采样率 | ADTS 与 LATM 均成功（**【测试】** 两侧大量往返用例） | 配置已含类型/采样率/声道数，库层面语义自洽；实际解码正确性未在本库验证 |
| HE-AAC v1/v2（SBR/PS，双采样率） | 仅 LATM/MP4 的 ASC 形式成功（**【测试】** SBR/PS 用例）；写 ADTS 时 PS 被拒、SBR 降级为 LC 头 | 双采样率与扩展类型只有保留在 ASC 中才不丢；转成 ADTS 后扩展语义在配置层消失，能否靠 AU 内 fill 元素恢复取决于外部解码器，超出本库证明范围 |
| channelConfig=0，AU 以 PCE 开头 | ADTS 头、ASC、LATM SMC 均解析成功并保留 0（**【测试】** `adts_test.go` 四个 `channel_config_0_*` 用例；【实验】SMC ch0 往返） | 需对每个 AU 调 `ParsePCEFromRawDataBlock` 才拿得到声道数与元素构成（**【测试】** 立体声/5.1 PCE 用例）；MP4/fMP4 写框在此之前就拒绝 |
| channelConfig=0，无带内 PCE，单元素隐式布局（单 SCE/CPE/LFE） | 同上 | `CountChannelsFromRawDataBlock` 可返回 1 或 2（**【测试】** `count_channels_test.go`）；布局方位语义靠 ISO 默认顺序，库不记录 |
| channelConfig=0，无带内 PCE，多元素隐式布局（如 SCE+CPE+LFE） | 头部同样成功 | **当前辅助函数只数第一个元素即返回，声道数偏小**（【实验】返回 1 而非 4）；库无法证明正确声道含义，需调用方另行完整解析 |
| PCE 位于 ASC 的 GASpecificConfig 内 | tag 高位为 0 时解析“成功”但 PCE 被忽略；tag 高位为 1 时被 `extensionFlag` 拒绝（【实验】） | 配置结构体无任何声道信息，且 Marshal 不回写 PCE；经 ASC 处理后该布局信息不可恢复 |
| 转义采样率（0xF+24 位） | LATM/MP4 成功（**【测试】** 53000 用例） | 写 ADTS 被拒，不存在 ADTS 形式的兼容输出 |
| 带 CRC 的 ADTS | 解析成功（**【测试】** `casesADTSCRC`） | 重封装修复必丢 CRC；库不校验 CRC，载荷语义不受影响 |
| LATM 多 program/多层、useSameConfig、useSameStreamMux | SMC/AME 解析成功（**【测试】** `audio_mux_element_test.go` 多 program 用例） | `useSameStreamMux=1` 的元素必须有外部缓存配置，否则整元素不可解析（【实验】`no StreamMuxConfig available`）；共享层以 `nil` ASC 表达，需调用方自行回溯 |
| LATM frameLengthType≠0 / OtherDataPresent / audioMuxVersion=1 | 元素级解析被拒或（Marshal FLT=1）panic（2.3 节） | 不满足头部成功前提，无后续解释可言 |

### 2.5 测试材料证据清单

- `adts_test.go`：普通 LC/SSR 帧、多帧、CRC 帧、四种 `channel_config=0`（CPE/SCE/LFE/PCE 开头 AU）、全部错误路径、Marshal 往返与 fuzz。
- `audio_specific_config_test.go`：LC 单声道/5.1/立体声/8 声道、53000 转义频率、coreCoderDelay、`channelConfig=0`、SBR、PS、Marshal 错误与 fuzz。
- `stream_mux_config_test.go`：LC、SBR、PS 嵌入 SMC、otherData+CRC、截断 SMC、往返与 fuzz。
- `audio_mux_element_test.go`：多 program/多层、`useSameStreamMux`、`MuxConfigPresent=false`（外部供配置）与 fuzz。
- `program_config_element_test.go` / `count_channels_test.go`：PCE 解析与隐式元素计数的全部已证明能力边界。
- `reader_test.go`（`"mpeg-4 audio LATM"` 用例，约 556 行起）：双 program LATM 轨道的端到端建轨与数据回调。

---

## 3. 结论

1. **双采样率并存**时，LATM/MP4 的 ASC 路径能完整保留核心采样率、扩展输出采样率与 SBR/PS 扩展类型（含转义频率），但要求核心类型为 LC；ADTS 头没有这些字段，向 ADTS 转换时 PS 被直接拒绝、SBR 被静默降级为 LC 配置，扩展采样率丢失。
2. **PCE 声道（channelConfig=0）** 在三种配置载体里都只保留“0”这个标记，声道数必须事后从 AU 恢复：带内 PCE 走 `ParsePCEFromRawDataBlock`（仅 PCE 在 AU 首位时有效），隐式布局走 `CountChannelsFromRawDataBlock`（当前只数首个声道元素，多元素布局结果偏小）；ASC 内嵌 PCE 不被解析、不可回写，部分取值还会被 `extensionFlag` 误拒。MP4/fMP4 写框对 channelConfig=0 整体拒绝。
3. **配置复用**分两层：LATM 元素级 `useSameStreamMux=1` 缺外部配置时整元素不可解析，层间 `useSameConfig=1` 以 `nil` 指针表达共享；mpegts 轨道只固化第一个 SMC，库不承担配置缓存与更新。ADTS 无复用机制，每帧自带头部。
4. 可被源码证明的**安全兼容区**是：AAC-LC、表内 13 种采样率（或仅在 LATM/MP4 中使用转义频率）、channelConfig 1–7、单 program 单 layer、`frameLengthType=0`、`audioMuxVersion=0`、`allStreamsSameTimeFraming=1`、`OtherDataPresent=false`。此区域内 ADTS↔LATM 的配置与 AU 载荷均可往返（CRC 除外）。超出该区域时，报告已逐项区分“头部即被拒绝”“封装修复期 panic”“头部成功但配置语义降级/声道数不可得”三类后果；在没有真实解码结果的前提下，不对降级后的实际出声效果作进一步结论。
