package com.antigravity.gateway.data

/** 环境变量在界面上的控件类型。 */
enum class EnvVarType { TEXT, INT, BOOL, SELECT }

data class EnvVarOption(val value: String, val label: String)

/**
 * 单个环境变量的元数据。
 *
 * 说明：UPSTREAM_BASE_URL / UPSTREAM_API_KEY / API_KEY / PORT 由主界面（供应商与端口）管理，
 * ENV_FILE 在 App 内不适用，均不在此列表中；Go 侧 androidbridge 也会拒绝这四个字段的覆盖。
 */
data class EnvVarDef(
    val key: String,
    val label: String,
    val description: String,
    val group: String,
    val type: EnvVarType,
    val default: String,
    val secret: Boolean = false,
    val options: List<EnvVarOption> = emptyList(),
    val min: Int = 0,
    val max: Int = 0,
    val unit: String = ""
)

data class EnvVarGroup(val id: String, val name: String, val desc: String)

object EnvVarSchema {

    val groups: List<EnvVarGroup> = listOf(
        EnvVarGroup("upstream", "上游服务", "上游连接池与鉴权模式"),
        EnvVarGroup("native", "原生流式", "Vertex/Gemini 原生流式传输（Preview）"),
        EnvVarGroup("listen", "网关监听", "监听地址（端口请在主界面修改）"),
        EnvVarGroup("downstream", "下游访问", "静态下游 Key 列表"),
        EnvVarGroup("admin", "管理与安全", "管理接口鉴权与 Key 存储"),
        EnvVarGroup("wrapper", "防截断策略", "合成传输协议与格式恢复行为"),
        EnvVarGroup("perf", "性能与并发", "请求大小与并发限制"),
        EnvVarGroup("stream", "流式缓冲", "SSE 流式输出缓冲参数"),
        EnvVarGroup("misc", "日志与其他", "停机、缓存、日志与代理信任")
    )

    val vars: List<EnvVarDef> = listOf(
        // ---------------- 上游服务 ----------------
        EnvVarDef(
            key = "UPSTREAM_AUTH_MODE", label = "上游鉴权模式", group = "upstream",
            type = EnvVarType.SELECT, default = "bearer",
            description = "bearer 携带上游 Key；none 适用于上游免鉴权的本地代理。",
            options = listOf(
                EnvVarOption("bearer", "bearer（携带上游密钥）"),
                EnvVarOption("none", "none（上游免鉴权）")
            )
        ),
        EnvVarDef(
            key = "UPSTREAM_TIMEOUT_MS", label = "上游请求超时", group = "upstream",
            type = EnvVarType.INT, default = "600000", min = 1000, max = 600000, unit = "ms",
            description = "上游单次请求超时时间（毫秒），默认 10 分钟。"
        ),
        EnvVarDef(
            key = "UPSTREAM_MAX_IDLE_CONNS", label = "连接池最大空闲连接", group = "upstream",
            type = EnvVarType.INT, default = "2048", min = 1, max = 100000,
            description = "上游 HTTP 连接池全局最大空闲连接数。"
        ),
        EnvVarDef(
            key = "UPSTREAM_MAX_IDLE_CONNS_PER_HOST", label = "每 Host 最大空闲连接", group = "upstream",
            type = EnvVarType.INT, default = "512", min = 1, max = 100000,
            description = "每个上游 Host 的最大空闲连接数。"
        ),
        EnvVarDef(
            key = "UPSTREAM_MAX_CONNS_PER_HOST", label = "每 Host 最大并发连接", group = "upstream",
            type = EnvVarType.INT, default = "512", min = 1, max = 100000,
            description = "每个上游 Host 的最大并发连接数。"
        ),

        // ---------------- 原生流式 ----------------
        EnvVarDef(
            key = "NATIVE_STREAM", label = "原生流式传输", group = "native",
            type = EnvVarType.BOOL, default = "false",
            description = "开启后可翻译的纯文本流式请求改用上游原生 streamGenerateContent（streamFunctionCallArguments），每段 partialArgs 立即还原为正文下发，不再等参数完整后一次性返回。仅适用于上游支持 Vertex/Gemini 原生协议时（Preview）。"
        ),
        EnvVarDef(
            key = "NATIVE_STREAM_URL_TEMPLATE", label = "原生流式地址模板", group = "native",
            type = EnvVarType.TEXT, default = "",
            description = "可选。{model} 会被替换为模型名（自动去除 google/ 前缀）。留空默认从上游基址推导 Vertex publishers 路径；Gemini API 示例：https://generativelanguage.googleapis.com/v1beta/models/{model}:streamGenerateContent?alt=sse"
        ),
        EnvVarDef(
            key = "NATIVE_STREAM_AUTH_STYLE", label = "原生接口鉴权方式", group = "native",
            type = EnvVarType.SELECT, default = "bearer",
            description = "bearer 使用 Authorization: Bearer；goog 使用 x-goog-api-key 头（Gemini Developer API 风格）。",
            options = listOf(
                EnvVarOption("bearer", "bearer（Authorization 头）"),
                EnvVarOption("goog", "goog（x-goog-api-key 头）")
            )
        ),

        // ---------------- 网关监听 ----------------
        EnvVarDef(
            key = "HOST", label = "监听地址", group = "listen",
            type = EnvVarType.SELECT, default = "0.0.0.0",
            description = "0.0.0.0 允许局域网设备访问；127.0.0.1 仅本机可访问。",
            options = listOf(
                EnvVarOption("0.0.0.0", "0.0.0.0（所有网卡，推荐）"),
                EnvVarOption("127.0.0.1", "127.0.0.1（仅本机）")
            )
        ),

        // ---------------- 下游访问 ----------------
        EnvVarDef(
            key = "DOWNSTREAM_KEYS_JSON", label = "静态下游 Key 列表", group = "downstream",
            type = EnvVarType.TEXT, secret = true, default = "",
            description = "JSON 数组形式的静态下游 Key，如 [{\"id\":\"a\",\"key\":\"sk-...\",\"name\":\"A\"}]。留空表示不使用。"
        ),

        // ---------------- 管理与安全 ----------------
        EnvVarDef(
            key = "ADMIN_API_KEY", label = "管理接口密钥", group = "admin",
            type = EnvVarType.TEXT, secret = true, default = "admin-secret-key-12345",
            description = "/admin/keys 等管理接口的 Bearer 鉴权密钥，自定义时至少 8 个字符。"
        ),
        EnvVarDef(
            key = "KEY_HMAC_SECRET", label = "Key 签名密钥", group = "admin",
            type = EnvVarType.TEXT, secret = true, default = "",
            description = "下游 Key 的 HMAC-SHA256 签名密钥，自定义时至少 16 个字符。"
        ),
        EnvVarDef(
            key = "KEY_DB_PATH", label = "Key 数据库路径", group = "admin",
            type = EnvVarType.TEXT, default = ":memory:",
            description = "动态 Key 存储路径。App 默认使用内存数据库（重启后动态 Key 清空）；主界面的全局下游 Key 不受影响。"
        ),

        // ---------------- 防截断策略 ----------------
        EnvVarDef(
            key = "WRAPPER_MODE", label = "包装模式", group = "wrapper",
            type = EnvVarType.SELECT, default = "prefer",
            description = "prefer 自适应注入合成传输协议；required 强制要求；off 为紧急回退到原生透传。",
            options = listOf(
                EnvVarOption("prefer", "prefer（自适应注入，推荐）"),
                EnvVarOption("required", "required（强制合成协议）"),
                EnvVarOption("off", "off（紧急透明透传回滚）")
            )
        ),
        EnvVarDef(
            key = "RECOVERY_POLICY", label = "格式恢复策略", group = "wrapper",
            type = EnvVarType.SELECT, default = "repair",
            description = "针对未闭合 JSON / Markdown 围栏等格式问题的处理策略。",
            options = listOf(
                EnvVarOption("repair", "repair（本地确定性修复）"),
                EnvVarOption("repair_then_retry", "repair_then_retry（修复失败后安全单次重试）"),
                EnvVarOption("fail", "fail（直接报错）")
            )
        ),
        EnvVarDef(
            key = "UPSTREAM_EMPTY_RETRIES", label = "上游空回重试次数", group = "wrapper",
            type = EnvVarType.INT, default = "3", min = 0, max = 10,
            description = "上游完全没有任何输出时的自动重试次数。"
        ),
        EnvVarDef(
            key = "CONTROL_MESSAGE_ROLE", label = "控制提示词角色", group = "wrapper",
            type = EnvVarType.SELECT, default = "system",
            description = "注入合成协议控制提示词时使用的角色。",
            options = listOf(
                EnvVarOption("system", "system"),
                EnvVarOption("developer", "developer")
            )
        ),
        EnvVarDef(
            key = "CONTROL_MESSAGE_POSITION", label = "控制提示词位置", group = "wrapper",
            type = EnvVarType.SELECT, default = "tail",
            description = "控制提示词的注入位置。",
            options = listOf(
                EnvVarOption("tail", "tail（末尾强生效，推荐）"),
                EnvVarOption("head", "head（开头）"),
                EnvVarOption("system_tail", "system_tail（系统提示末尾）")
            )
        ),
        EnvVarDef(
            key = "SYNTHETIC_TOOL_PREFIX", label = "合成工具名前缀", group = "wrapper",
            type = EnvVarType.TEXT, default = "agw_emit_",
            description = "动态合成传输工具的名称前缀（实际工具名为 前缀 + 96-bit 随机 Nonce）。"
        ),
        EnvVarDef(
            key = "SYNTHETIC_TOOL_STRICT", label = "严格 Tool Schema", group = "wrapper",
            type = EnvVarType.BOOL, default = "false",
            description = "是否向模型发送严格（Strict）Tool Schema 约束。部分上游支持不佳，出问题时可关闭。"
        ),
        EnvVarDef(
            key = "TEXT_MODEL_PATTERN", label = "文本模型匹配正则", group = "wrapper",
            type = EnvVarType.TEXT, default = "",
            description = "可选正则：自定义判定为文本对话模型并启用防截断保护。留空使用内置规则。"
        ),
        EnvVarDef(
            key = "NON_TEXT_MODEL_PATTERN", label = "非文本模型匹配正则", group = "wrapper",
            type = EnvVarType.TEXT, default = "",
            description = "可选正则：自定义判定为生图/语音/嵌入模型并跳过包装直接透传。留空使用内置规则。"
        ),

        // ---------------- 性能与并发 ----------------
        EnvVarDef(
            key = "MAX_REQUEST_BYTES", label = "最大请求大小", group = "perf",
            type = EnvVarType.INT, default = "16777216", min = 1024, max = 104857600, unit = "bytes",
            description = "最大允许请求体大小（字节，默认 16MB）。"
        ),
        EnvVarDef(
            key = "MAX_RESPONSE_BYTES", label = "最大响应大小", group = "perf",
            type = EnvVarType.INT, default = "16777216", min = 1024, max = 104857600, unit = "bytes",
            description = "最大允许响应体大小（字节，默认 16MB）。"
        ),
        EnvVarDef(
            key = "MAX_CONCURRENT_REQUESTS", label = "全局最大并发请求", group = "perf",
            type = EnvVarType.INT, default = "1024", min = 1, max = 100000,
            description = "全局最大并发请求数，超出后快速拒绝（过载保护）。"
        ),
        EnvVarDef(
            key = "MAX_CONCURRENT_REQUESTS_PER_KEY", label = "单 Key 最大并发", group = "perf",
            type = EnvVarType.INT, default = "64", min = 1, max = 100000,
            description = "单个下游 API Key 的最大并发请求数。"
        ),
        EnvVarDef(
            key = "REQUEST_QUEUE_TIMEOUT_MS", label = "请求排队超时", group = "perf",
            type = EnvVarType.INT, default = "50", min = 0, max = 10000, unit = "ms",
            description = "高并发下请求排队最大等待时间（毫秒）。"
        ),

        // ---------------- 流式缓冲 ----------------
        EnvVarDef(
            key = "STREAM_SIDE_BUFFER_BYTES", label = "流式 Side Buffer", group = "stream",
            type = EnvVarType.INT, default = "0", min = 0, max = 10485760, unit = "bytes",
            description = "流式旁路缓冲大小（字节）。默认 0 即极速直发、零附加延迟。"
        ),
        EnvVarDef(
            key = "STREAM_REPAIR_BUFFER_BYTES", label = "流式修复缓冲区", group = "stream",
            type = EnvVarType.INT, default = "1048576", min = 1024, max = 10485760, unit = "bytes",
            description = "流式格式修复缓冲区大小（字节，默认 1MB）。"
        ),
        EnvVarDef(
            key = "STREAM_FLUSH_INTERVAL_MS", label = "流式刷新间隔", group = "stream",
            type = EnvVarType.INT, default = "0", min = 0, max = 10, unit = "ms",
            description = "流式输出刷新间隔（毫秒）。默认 0 即刻刷新。"
        ),

        // ---------------- 日志与其他 ----------------
        EnvVarDef(
            key = "SHUTDOWN_TIMEOUT_MS", label = "优雅停机超时", group = "misc",
            type = EnvVarType.INT, default = "30000", min = 1000, max = 300000, unit = "ms",
            description = "收到终止信号后等待在途请求完成的最长时间（毫秒）。"
        ),
        EnvVarDef(
            key = "MODELS_CACHE_TTL_MS", label = "模型列表缓存", group = "misc",
            type = EnvVarType.INT, default = "30000", min = 0, max = 86400000, unit = "ms",
            description = "/v1/models 上游模型列表的缓存有效期（毫秒）。"
        ),
        EnvVarDef(
            key = "LOG_LEVEL", label = "日志级别", group = "misc",
            type = EnvVarType.SELECT, default = "info",
            description = "日志输出详细程度。",
            options = listOf(
                EnvVarOption("debug", "debug"),
                EnvVarOption("info", "info"),
                EnvVarOption("warn", "warn"),
                EnvVarOption("error", "error")
            )
        ),
        EnvVarDef(
            key = "TRUST_PROXY", label = "信任反向代理", group = "misc",
            type = EnvVarType.BOOL, default = "false",
            description = "是否信任反向代理传递的 X-Forwarded-For 真实客户端 IP。"
        )
    )

    private val byKey: Map<String, EnvVarDef> = vars.associateBy { it.key }

    fun byKey(key: String): EnvVarDef? = byKey[key]

    fun isSecret(key: String): Boolean = byKey[key]?.secret == true

    /** 当前生效值：优先取用户覆盖，否则取内置默认值。 */
    fun effectiveValue(def: EnvVarDef, overrides: Map<String, String>): String {
        val v = overrides[def.key]
        return if (v.isNullOrEmpty()) def.default else v
    }

    /** 把环境变量覆盖序列化为 JSON 字符串，供 JNI 桥透传给 Go 引擎。 */
    fun buildOverridesJson(overrides: Map<String, String>): String {
        val obj = org.json.JSONObject()
        for ((k, v) in overrides) {
            if (v.isNotEmpty() && byKey(k) != null) {
                obj.put(k, v)
            }
        }
        return obj.toString()
    }
}
