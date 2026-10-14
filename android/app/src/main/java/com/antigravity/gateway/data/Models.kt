package com.antigravity.gateway.data

import com.antigravity.gateway.util.NetworkUtils
import java.util.UUID

data class Provider(
    val id: String = UUID.randomUUID().toString(),
    val name: String,
    val upstreamUrl: String = "",
    val upstreamKey: String = ""
)

data class GatewayConfig(
    val schemaVersion: Int = 1,
    val currentProviderId: String,
    val downstreamKey: String,
    val port: Int = NetworkUtils.GATEWAY_PORT,
    val providers: List<Provider>,
    /** 高级配置：环境变量覆盖（仅含 EnvVarSchema 中的变量，空值 = 使用默认）。 */
    val envOverrides: Map<String, String> = emptyMap()
) {
    fun getCurrentProvider(): Provider? {
        return providers.firstOrNull { it.id == currentProviderId } ?: providers.firstOrNull()
    }

    /** 已自定义（与默认值不同）的环境变量数量，用于主界面入口提示。 */
    fun customizedEnvCount(): Int = envOverrides.count { it.value.isNotEmpty() }
}
