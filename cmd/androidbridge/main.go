//go:build android || cgo_jni

package main

/*
#include <jni.h>
#include <stdlib.h>

static const char* get_jstring_chars(JNIEnv *env, jstring jstr) {
    if (!jstr) return NULL;
    return (*env)->GetStringUTFChars(env, jstr, NULL);
}

static void release_jstring_chars(JNIEnv *env, jstring jstr, const char *chars) {
    if (jstr && chars) {
        (*env)->ReleaseStringUTFChars(env, jstr, chars);
    }
}

static jstring create_jstring(JNIEnv *env, const char *str) {
    if (!str) return NULL;
    return (*env)->NewStringUTF(env, str);
}
*/
import "C"
import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"antigravity-gateway/pkg/gateway"
)

var (
	engineMu     sync.Mutex
	activeEngine *gateway.Engine
	lastError    string
)

func extractJString(env *C.JNIEnv, jstr C.jstring) string {
	cStr := C.get_jstring_chars(env, jstr)
	if cStr == nil {
		return ""
	}
	defer C.release_jstring_chars(env, jstr, cStr)
	return C.GoString(cStr)
}

func createJString(env *C.JNIEnv, str string) C.jstring {
	cStr := C.CString(str)
	defer C.free(unsafe.Pointer(cStr))
	return C.create_jstring(env, cStr)
}

// primaryEnvKeys 是由 Android 主界面（供应商 / 端口 / 下游 Key）管理的字段，
// 高级配置中的环境变量覆盖不允许篡改它们，保证单一事实来源。
var primaryEnvKeys = map[string]bool{
	"UPSTREAM_BASE_URL": true,
	"UPSTREAM_API_KEY":  true,
	"API_KEY":           true,
	"PORT":              true,
}

// mergeEnvOverrides 把界面传来的 JSON 环境变量覆盖合并进基础 envMap。
// 规则：
//   - 主界面管理的四个字段忽略覆盖；
//   - 空字符串表示「恢复默认」，从 map 中删除该键；
//   - 未知键无害透传（config.Load 只会读取已知变量）。
func mergeEnvOverrides(base map[string]string, rawJSON string) error {
	raw := strings.TrimSpace(rawJSON)
	if raw == "" || raw == "{}" {
		return nil
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return fmt.Errorf("invalid env overrides JSON: %w", err)
	}
	for k, v := range overrides {
		if primaryEnvKeys[k] {
			continue
		}
		if strings.TrimSpace(v) == "" {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	return nil
}

//export Java_com_antigravity_gateway_bridge_GatewayBridge_nativeStartGateway
func Java_com_antigravity_gateway_bridge_GatewayBridge_nativeStartGateway(
	env *C.JNIEnv,
	clazz C.jclass,
	jUpstreamUrl C.jstring,
	jUpstreamKey C.jstring,
	jDownstreamKey C.jstring,
	jPort C.jint,
	jEnvJson C.jstring,
) C.jint {
	engineMu.Lock()
	defer engineMu.Unlock()

	if activeEngine != nil && activeEngine.IsRunning() {
		return 0 // Already running
	}

	upstreamUrl := extractJString(env, jUpstreamUrl)
	upstreamKey := extractJString(env, jUpstreamKey)
	downstreamKey := extractJString(env, jDownstreamKey)
	envJson := extractJString(env, jEnvJson)
	port := int(jPort)
	if port <= 0 {
		port = 38472
	}

	if upstreamUrl == "" || upstreamKey == "" {
		lastError = "upstream URL and upstream Key are required"
		return 1
	}

	envMap := map[string]string{
		"UPSTREAM_BASE_URL":  upstreamUrl,
		"UPSTREAM_API_KEY":   upstreamKey,
		"UPSTREAM_AUTH_MODE": "bearer",
		"API_KEY":            downstreamKey,
		"KEY_DB_PATH":        ":memory:",
		"HOST":               "0.0.0.0",
		"PORT":               strconv.Itoa(port),
		"WRAPPER_MODE":       "prefer",
		"RECOVERY_POLICY":    "repair",
	}

	// 应用高级配置（环境变量）覆盖
	if err := mergeEnvOverrides(envMap, envJson); err != nil {
		lastError = err.Error()
		return 3
	}

	getenv := func(key string) string {
		return envMap[key]
	}

	engine, err := gateway.StartEngine(getenv)
	if err != nil {
		lastError = err.Error()
		return 2
	}

	activeEngine = engine
	lastError = ""
	return 0
}

//export Java_com_antigravity_gateway_bridge_GatewayBridge_nativeStopGateway
func Java_com_antigravity_gateway_bridge_GatewayBridge_nativeStopGateway(
	env *C.JNIEnv,
	clazz C.jclass,
) C.jint {
	engineMu.Lock()
	defer engineMu.Unlock()

	if activeEngine == nil {
		return 0
	}

	err := activeEngine.Stop()
	activeEngine = nil
	if err != nil {
		lastError = err.Error()
		return 1
	}
	lastError = ""
	return 0
}

//export Java_com_antigravity_gateway_bridge_GatewayBridge_nativeIsRunning
func Java_com_antigravity_gateway_bridge_GatewayBridge_nativeIsRunning(
	env *C.JNIEnv,
	clazz C.jclass,
) C.jboolean {
	engineMu.Lock()
	defer engineMu.Unlock()

	if activeEngine != nil && activeEngine.IsRunning() {
		return 1
	}
	return 0
}

//export Java_com_antigravity_gateway_bridge_GatewayBridge_nativeGetLastError
func Java_com_antigravity_gateway_bridge_GatewayBridge_nativeGetLastError(
	env *C.JNIEnv,
	clazz C.jclass,
) C.jstring {
	engineMu.Lock()
	errStr := lastError
	engineMu.Unlock()

	return createJString(env, errStr)
}

func main() {}
