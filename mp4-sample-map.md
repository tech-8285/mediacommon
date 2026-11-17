# 渐进 MP4（pmp4）样本表如何确定媒体字节位置与播放区间

分析对象：`pkg/formats/pmp4/`（`presentation.go`、`track.go`、`sample.go`）及其测试
`presentation_test.go`。该包读取/写出非分片（progressive）MP4：`ftyp + moov + mdat` 结构。

## 0. 样本表总览

| 盒子 | 角色 | 读取处理（presentation.go） | 写出处理（track.go） |
|---|---|---|---|
| `stts` | 每样本时长（游程编码） | 展开为 `Samples[i].Duration`，样本总数由此确定（L211-221） | 按 Duration 游程压缩（L357-383） |
| `stss` | 关键帧（sync sample）序号表 | 先全部置 `IsNonSyncSample=true`，再按序号清除（L252-261） | 全同步时省略不写（L385-405） |
| `ctts` | 每样本合成偏移 PTSOffset | 按序填入 `Samples[i].PTSOffset`（L274-285） | 游程压缩，固定写 version 1（L407-436） |
| `stsc` | 样本→块映射（每块样本数可变） | 展开为 chunk 列表，每块记录 sampleCount（L298-333） | 由字节偏移邻接性反推块边界（L438-481） |
| `stsz` | 每样本字节大小 | 存入 `curSampleSizes`，支持恒定大小（L335-359） | 恒写逐样本表（SampleSize=0）（L483-496） |
| `stco` | 块→文件字节偏移 | 与 chunk 列表配对，计算每样本绝对偏移（L361-411） | 先写占位，moov 结束后回填（L575-584） |
| `mdhd` | 轨道 TimeScale 与媒体时长 | TimeScale 非零校验；时长用于求 TimeOffset（L164-182, L228） | duration = sampleDuration + TimeOffset，超 32 位写 v1（L136-147） |
| `elst` | 编辑列表 | **只校验存在性，内容完全忽略**（L148-153） | 由 TimeOffset 生成空编辑或 media_time 平移（L322-355） |

样本的身份由它在 `stts` 展开序列中的**下标**唯一确定；`stss`、`ctts`、`stsz` 都按下标与
之对齐，`stsc`+`stco` 也按下标顺序消费。这是所有对应关系的枢纽。

## 1. 负载位置、解码时刻、显示时刻如何保持对应

### 1.1 负载位置（字节偏移）

读取侧在 `stco` 分支（presentation.go L380-409）完成三级映射：

```
stsc 展开 → chunk 列表（每块 sampleCount）
stco      → 每块起始文件偏移 chunk.offset
stsz      → 每样本大小 curSampleSizes[i]

样本 i 的偏移 = 所在块偏移 + 块内前序样本大小之和
```

每个样本得到一个闭包 `GetPayload`，惰性 `Seek(sampleOffset)` 后读 `PayloadSize` 字节
（L395-404）。因为按绝对偏移寻址，`mdat` 可以出现在 `moov` 之前（测试 "h264 mdat
before moov" 已证实，`free`/`uuid`/`sdtp`/`ilst` 等未知盒子一律跳过）。

**每块样本数变化**由 `stsc` 多条目表达：条目 i 生效的块数 = 下一条目 `FirstChunk` 之差，
末条目则摊满剩余样本（L307-315）。写出侧反过来：`sortSamples` 先给每样本分配全局
`offset`，`marshalSTSC`/`marshalSTCO` 把"同轨道且偏移相邻"的连续样本并成块
（track.go L448-463, L501-512），再做一次相邻等值合并压缩（L466-473）。因此块大小的
变化是音视频样本交错结果的自然产物，不需要外部指定。

### 1.2 解码时刻（DTS）

`stts` 只存时长，样本 i 的解码时刻是隐式前缀和：`DTS_i = Σ Duration[0..i-1]`，单位为
轨道自己的 `TimeScale`。读取侧不显式计算 DTS，只保留 `Duration`；写出侧
`sortSamples`（presentation.go L456-502）为每轨维护 `elapsed` 累加器，初值为
`track.TimeOffset`，每消费一个样本加 `Duration`——这就是该样本在呈现时间轴上的解码
时刻。

### 1.3 显示时刻（PTS）

`PTS_i = DTS_i + PTSOffset_i`，`PTSOffset` 来自 `ctts`。读取用
`ctts.GetSampleOffset(i)`（L282），对 version 0（无符号）和 version 1（有符号）统一
处理——测试 "ctts version 0" 证实 v0 文件可读；写出恒用 v1，因此负 PTSOffset（B 帧
重排导致显示早于解码）可以往返保留（"standard" 用例中 PTSOffset=-45000 的样本）。

### 1.4 时间尺度不同的轨道如何对齐

各轨 `TimeScale` 独立（mdhd），互不影响样本表内的算术；只有两处需要跨轨统一：

- **写出交错**：`sortSamples` 用 `durationMp4ToGo`（L23-28）把各轨 `elapsed` 换算成
  `time.Duration`（纳秒）再取最小者，决定下一个进入 `mdat` 的样本（L476-489）。相等
  时排在前面的轨道优先（严格小于比较）。
- **呈现时长**：`tkhd`/`mvhd` 时长统一换算到全局 1000 时间尺度
  （track.go L84，presentation.go L18）。

于是同一样本的三元组——下标（身份）→（stsc/stco/stsz）字节位置、（stts 前缀和）解码
时刻、（+ctts）显示时刻——在读取与写出两条路径上都按下标对齐，轨道时间尺度差异只
影响跨轨排序与头部时长的单位换算，不影响轨道内部对应关系。

### 1.5 关键帧标记与轨道起始偏移分别约束什么

- **关键帧标记（stss / IsNonSyncSample）**：只约束**随机访问的落点**——seek/快进必须
  落在 sync sample 上才能开始解码。它不参与时间戳、排序或字节定位。stss 缺失按规范
  视为全部同步（写出侧据此省略 stss，track.go L386-389 引 ISO 14496-12）；stss 存在
  时读取侧先假定全部非同步再按表清除（presentation.go L252-261）。测试 "av1 with
  free and sdtp" 的文件同时含 `stss` 与 `sdtp`，期望值中的 `IsNonSyncSample` 只反映
  stss——**sdtp 的样本依赖标志被静默忽略**（间接证据：该用例字节中 sdtp 存在但无对
  应读取分支）。
- **轨道起始偏移（TimeOffset）**：约束**轨道媒体在呈现时间轴上的起点**，不影响轨道内
  样本的相对 DTS。读取：`TimeOffset = mdhd.duration − Σstts`（L228）。写出分两种：
  - 正偏移（轨道晚出现）：elst 写两条目——前导空编辑（`MediaTime=-1` 的暂停段）+
    整段媒体（track.go L323-341），`mdhd.duration = sampleDuration + TimeOffset`；
  - 负偏移（媒体开头被裁）：elst 写单条目，`MediaTime = -TimeOffset`（L344-354），
    `mdhd.duration = sampleDuration − |TimeOffset|`。
  它同时是 `sortSamples` 交错排序的初始 `elapsed`（presentation.go L468-470），决定
  该轨样本在 `mdat` 中的相对先后。

  **已证实的怪异点**：负偏移时写出的 elst 段长为 `sampleDuration + |TimeOffset|`
  （track.go L347-348）。"standard" 用例轨道 1（TimeOffset=-90000，3×90000 样本）
  的编码字节中 elst 段长 = 0x00000fa0 = 4000ms、MediaTime = 90000，而 tkhd 时长 =
  0x07d0 = 2000ms、mdhd 时长 = 0x0002bf20 = 180000（2s）——段长超出 tkhd 时长与剩
  余媒体长度。本库读取侧不解析 elst，往返不受影响；第三方播放器如何解释该组合未在
  测试中断言。

## 2. 记录矛盾与超出支持范围时的行为

### 2.1 明确拒绝（返回错误，Unmarshal 失败）

| 矛盾/超限 | 错误 | 位置 |
|---|---|---|
| stsz 样本数 ≠ stts 展开数 | `invalid stsz` | presentation.go L346-348 |
| stsc 条目 SamplesPerChunk == 0 | `invalid stsc` | L302-304 |
| stsc 末条目不能整除剩余样本 | `invalid stsc` | L311-313 |
| stsc 累计样本数超过 stts 总数 | `invalid stsc` | L322-324 |
| stsc 展开块数超 maxChunks | `invalid stsc` | L317-319 |
| stco 条目数 ≠ stsc 展开块数 | `invalid stco` | L372-374 |
| 样本大小为 0 | `invalid sample size` | L389-391 |
| stss 序号超出样本数 | `invalid stss` | L257-259 |
| ctts 条目累计超过样本数 | `invalid ctts` | L277-279 |
| stts 展开数超 maxSamples（30fps×2天） | `max samples reached` | L212-214 |
| mdhd timescale == 0 | `invalid timescale` | L175-177 |
| mdhd v1 时长使 TimeOffset 溢出 int32 | `invalid duration` | L232-237 |
| 出现 mvex（分片 MP4） | `fragmented MP4 files are not supported` | L413-415 |
| stco 之前未收到 stsz / 重复 stsz、stco | `unexpected box` | L336, L362 |
| 盒子顺序不符状态机 / 缺 stco 收尾 | `unexpected box` / `parse error` | 各分支, L423-425 |

注意 **co64（64 位块偏移）没有读取分支**：含 co64 而无 stco 的文件会因 `stcoReceived`
恒为 false 在下一 trak 或文件结尾处报错——大文件（>4GB 偏移）被间接但明确地拒绝。

### 2.2 信息丢失（静默接受，语义被压扁或丢弃）

- **elst 内容完全忽略**（L148-153 只推进状态机）。多段编辑、变速编辑、与
  `mdhd−stts` 不一致的 media_time 全部被丢弃，编辑时间线被压扁成一个标量
  `TimeOffset = mdhd.duration − Σstts`。这是"编辑时间线超出当前支持范围"时的主要行
  为：不报错，但复杂编辑语义丢失。测试 "no edts" 证实 edts 整体缺失也可解析
  （TimeOffset 由 mdhd 与 stts 推出，该用例为 0）。
- **mdhd 时长与 stts 总和矛盾**不被视为矛盾：差值直接被解释为轨道起始偏移（L228）。
  v0 时长即使已环绕也不校验（L230-231 注释：旧版 Marshal 写出的 v0 时长可能已环
  绕）。测试 "long duration with wrapped mdhd version 0" 证实：真实媒体时长
  4536090000（>2^32）在 v0 mdhd 中环绕为 241122704，但 `int32(trackDuration) −
  int32(sampleDuration)` 的环绕相互抵消，仍恢复出 TimeOffset=90000。
- **ctts 条目数少于样本数**：剩余样本 PTSOffset 静默取 0（L274-285 只填前 n 个）。
- **未知 codec**：`codecBoxesReader.Codec == nil` 时整条轨道被静默丢弃
  （L74-78），再写入时该轨消失。
- **sdtp、ilst、uuid、free 等盒子**一律跳过，其承载的样本依赖/元数据语义丢失。
- **stsc 缺失**（代码可达、无测试）：chunk 列表为空时 stco 必须为 0 条目，解析"成
  功"但所有样本 PayloadSize=0、GetPayload=nil——负载定位信息整体丢失。

### 2.3 仍可保留的媒体语义

即使编辑列表被压扁，以下语义在读取后完整保留并可再写入：每样本时长（DTS 序列）、
PTS 偏移（含负值，ctts v1）、关键帧标记、逐样本字节大小与负载（经 GetPayload）、编
码参数（stsd 解析出的 codec 配置）、轨道 ID 与 TimeScale、以及 int32 范围内的轨道起
始偏移。丢失的仅是：复杂编辑结构、sdtp 依赖标志、未知盒子、未识别 codec 的轨道。

### 2.4 读取后再写入的兼容判据

由 fuzz 测试（presentation_test.go L3000-3034，`FuzzPresentationUnmarshal`：解析成
功 → 逐样本校验 → 必须能 Marshal）与实现约束归纳，一个文件能"读取→再写入"往返的条
件是：

1. 非分片（无 mvex），含 stco（32 位偏移，非 co64）；
2. 每轨 TimeScale ≠ 0，codec 被 `CodecBoxesReader` 识别（否则轨道被丢弃，写出时缺失）；
3. stsz 计数 == stts 展开数；stsc 一致（每块样本数非零、末块整除、总量不超限）；
   stss 序号在界内；ctts 累计 ≤ 样本数；
4. 每样本 PayloadSize > 0 且 GetPayload 恰好返回 PayloadSize 字节（fuzz 断言；
   写出侧 `marshalMdat` 不校验长度，长度不符会静默产出损坏文件——调用方责任）；
5. mdhd v1 时 `mdhd.duration − Σstts` 落在 int32 内（v0 环绕值可容忍）；
6. 样本总数 ≤ maxSamples、块数 ≤ maxChunks；
7. 写出侧额外约束：总负载须 < 4GB——`dataSize`、`sample.offset`、`stco` 均为
   uint32，mdat 头固定 8 字节无 largesize（presentation.go L591-593）；超限会静默环
   绕（无测试覆盖，见证据缺口）。

满足 1-6 的文件经本库读取后，其样本内容、时间戳（含 PTS 偏移与轨道起始偏移）、关键
帧结构可按上述语义等价地重新 multiplex 写出；字节级不一定相同（stsz 恒写逐样本表、
ctts 恒写 v1、stss 全同步时省略、elst 由 TimeOffset 重新生成、块边界由交错结果重新
划分），但样本表承载的媒体语义等价。

## 3. 已证实行为与证据缺口

**已证实（有测试或字节级证据）**：

- 样本位置三级映射与跨轨交错写出：全部 `casesPresentation` 用例（standard、long
  duration、no samples 等）做了解码↔编码双向断言。
- mdat 在 moov 前、free/uuid/sdtp/ilst 跳过："h264 mdat before moov"、"uuid"、"av1
  with free and sdtp"。
- ctts v0 读取："ctts version 0"；负 PTSOffset 往返："standard"。
- stsz 恒定大小（SampleSize≠0）读取："stsz with constant sample size"。
- mdhd v0 时长环绕仍恢复 TimeOffset："long duration with wrapped mdhd version 0"；
  v1 超长时长："long duration"（mdhd v1，duration=4536090000）。
- edts 缺失可解析："no edts"。
- GetPayload 惰性 seek 的错误传播：`TestPresentationGetPayloadSeekError`。
- 负 TimeOffset 的 elst 段长怪异值（4000ms > tkhd 2000ms）："standard" 编码字节。
- 解析成功 ⇒ 可再 Marshal 且样本负载完整：fuzz 测试断言。

**证据缺口（代码路径存在但无测试/未断言）**：

- stsc 缺失 + 空 stco 时解析"成功"但样本无负载（PayloadSize=0、GetPayload=nil）的
  行为无测试；fuzz 对解析成功者断言 PayloadSize≠0，但种子语料不含此类文件。
- 写出总负载 >4GB 时 uint32 偏移/mdat 长度环绕，无校验也无测试。
- `sortSamples` 跨轨比较先截断到纳秒（`durationMp4ToGo`），极端时间尺度下原本不等
  的 DTS 可能被判等而改变交错顺序，无测试。
- 负 TimeOffset 写出的 elst 组合（段长超出媒体长度）对第三方播放器的影响未验证。
- ctts 条目少于样本数时静默补 0 的行为无专门测试。
- sdtp 被忽略仅有间接证据（av1 用例中 sdtp 存在但期望值只来自 stss），无"仅靠 sdtp
  标记非同步样本"的用例。
