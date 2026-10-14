package com.antigravity.gateway.ui

import android.graphics.Typeface
import android.os.Bundle
import android.view.View
import android.widget.AdapterView
import android.widget.ArrayAdapter
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.Spinner
import android.widget.Switch
import android.widget.TextView
import android.widget.Toast
import androidx.activity.viewModels
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import com.google.android.material.dialog.MaterialAlertDialogBuilder
import kotlinx.coroutines.launch
import com.antigravity.gateway.R
import com.antigravity.gateway.data.EnvVarDef
import com.antigravity.gateway.data.EnvVarSchema
import com.antigravity.gateway.data.EnvVarType
import com.antigravity.gateway.databinding.ActivityEnvConfigBinding

/**
 * 高级配置（环境变量）界面。
 *
 * 以分组表单的形式展示 EnvVarSchema 中的全部可配置环境变量，
 * 修改后写入本地配置（敏感值加密存储），经 JNI 桥传给 Go 引擎，重启网关后生效。
 *
 * 上游地址 / 上游 Key / 下游 Key / 端口由主界面管理，不在此处重复配置。
 */
class EnvConfigActivity : AppCompatActivity() {

    private lateinit var binding: ActivityEnvConfigBinding
    private val viewModel: MainViewModel by viewModels()

    /** key -> 控件（EditText / Switch / Spinner） */
    private val controls = HashMap<String, View>()
    /** key -> 每行的「恢复默认」按钮 */
    private val resetButtons = HashMap<String, TextView>()
    /** key -> 程序正在同步控件值（此时不触发保存） */
    private val updating = HashMap<String, Boolean>()
    private var formBuilt = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityEnvConfigBinding.inflate(layoutInflater)
        setContentView(binding.root)

        binding.btnBack.setOnClickListener { finish() }
        binding.btnResetAll.setOnClickListener { confirmResetAll() }

        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.STARTED) {
                viewModel.uiState.collect { state ->
                    val overrides = state.config.envOverrides
                    if (!formBuilt) {
                        buildForm(overrides)
                        formBuilt = true
                    } else {
                        syncForm(overrides)
                    }
                    if (state.statusMessage.isNotEmpty()) {
                        Toast.makeText(this@EnvConfigActivity, state.statusMessage, Toast.LENGTH_SHORT).show()
                        viewModel.clearStatusMessage()
                    }
                }
            }
        }
    }

    // ---------------- 表单构建 ----------------

    private fun buildForm(overrides: Map<String, String>) {
        val container = binding.llEnvContainer
        container.removeAllViews()
        controls.clear()
        resetButtons.clear()
        updating.clear()

        for (group in EnvVarSchema.groups) {
            val vars = EnvVarSchema.vars.filter { it.group == group.id }
            if (vars.isEmpty()) continue

            container.addView(
                TextView(this).apply {
                    text = group.name
                    textSize = 14f
                    setTypeface(typeface, Typeface.BOLD)
                    setTextColor(color(R.color.text_primary))
                    setPadding(0, dp(14), 0, dp(2))
                }
            )
            container.addView(
                TextView(this).apply {
                    text = group.desc
                    textSize = 11.5f
                    setTextColor(color(R.color.text_secondary))
                    setPadding(0, 0, 0, dp(6))
                }
            )

            for (def in vars) {
                container.addView(createFieldRow(def, overrides))
            }
        }
    }

    private fun createFieldRow(def: EnvVarDef, overrides: Map<String, String>): View {
        val customized = overrides[def.key]?.isNotEmpty() == true

        val row = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            layoutParams = LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                LinearLayout.LayoutParams.WRAP_CONTENT
            )
        }

        // 标题行：标签 + 恢复默认
        val headerRow = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = android.view.Gravity.CENTER_VERTICAL
        }
        headerRow.addView(
            TextView(this).apply {
                text = def.label
                textSize = 15f
                setTextColor(color(R.color.text_primary))
                layoutParams = LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f)
            }
        )
        val reset = TextView(this).apply {
            text = getString(R.string.env_config_reset_field)
            textSize = 12.5f
            setTextColor(color(R.color.link))
            setPadding(dp(10), dp(6), dp(2), dp(6))
            visibility = if (customized) View.VISIBLE else View.GONE
            setOnClickListener { viewModel.resetEnvOverride(def.key) }
        }
        resetButtons[def.key] = reset
        headerRow.addView(reset)
        row.addView(headerRow)

        row.addView(
            TextView(this).apply {
                text = def.key
                textSize = 11f
                typeface = Typeface.MONOSPACE
                setTextColor(color(R.color.text_secondary))
                setPadding(0, dp(2), 0, 0)
            }
        )
        row.addView(
            TextView(this).apply {
                text = def.description + defaultSuffix(def)
                textSize = 12f
                setTextColor(color(R.color.text_secondary))
                setPadding(0, dp(2), 0, dp(6))
            }
        )

        row.addView(createControl(def, EnvVarSchema.effectiveValue(def, overrides)))

        // 分割线
        row.addView(
            View(this).apply {
                setBackgroundColor(color(android.R.color.darker_gray))
                layoutParams = LinearLayout.LayoutParams(
                    LinearLayout.LayoutParams.MATCH_PARENT,
                    1
                ).apply { topMargin = dp(12) }
            }
        )
        return row
    }

    private fun defaultSuffix(def: EnvVarDef): String =
        if (def.default.isNotEmpty()) "（默认: ${def.default}）" else ""

    private fun createControl(def: EnvVarDef, value: String): View {
        return when (def.type) {
            EnvVarType.BOOL -> {
                Switch(this).apply {
                    isChecked = value == "true" || value == "1" || value == "yes"
                    // 先设初值再挂监听，避免初始化回调触发保存
                    setOnCheckedChangeListener { _, checked ->
                        if (updating[def.key] == true) return@setOnCheckedChangeListener
                        viewModel.updateEnvOverride(def.key, if (checked) "true" else "false")
                    }
                }
            }

            EnvVarType.SELECT -> {
                val spinner = Spinner(this)
                spinner.adapter = ArrayAdapter(
                    this,
                    android.R.layout.simple_spinner_item,
                    def.options.map { it.label }
                ).apply {
                    setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item)
                }
                val current = value.ifEmpty { def.default }
                val idx = def.options.indexOfFirst { it.value == current }.coerceAtLeast(0)
                updating[def.key] = true
                spinner.setSelection(idx)
                updating[def.key] = false
                spinner.onItemSelectedListener = object : AdapterView.OnItemSelectedListener {
                    override fun onItemSelected(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                        if (updating[def.key] == true) return
                        viewModel.updateEnvOverride(def.key, def.options[position].value)
                    }

                    override fun onNothingSelected(parent: AdapterView<*>?) {}
                }
                spinner
            }

            else -> {
                EditText(this).apply {
                    setText(value)
                    textSize = 14f
                    setTextColor(color(R.color.text_primary))
                    setHintTextColor(color(R.color.text_secondary))
                    minHeight = dp(44)
                    isSingleLine = true
                    when (def.type) {
                        EnvVarType.INT -> {
                            inputType = android.text.InputType.TYPE_CLASS_NUMBER
                            hint = "默认 ${def.default}"
                        }
                        else -> {
                            inputType = if (def.secret) {
                                android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_VARIATION_PASSWORD
                            } else {
                                android.text.InputType.TYPE_CLASS_TEXT
                            }
                            if (value.isEmpty() && def.default.isNotEmpty()) {
                                hint = "默认 ${def.default}"
                            }
                        }
                    }
                    // 失焦时保存，避免逐字校验打扰输入
                    setOnFocusChangeListener { _, hasFocus ->
                        if (!hasFocus && updating[def.key] != true) {
                            viewModel.updateEnvOverride(def.key, text.toString())
                        }
                    }
                }
            }
        }.also { controls[def.key] = it }
    }

    // ---------------- 状态同步 ----------------

    private fun syncForm(overrides: Map<String, String>) {
        for (def in EnvVarSchema.vars) {
            val value = EnvVarSchema.effectiveValue(def, overrides)
            updating[def.key] = true
            when (val control = controls[def.key]) {
                is EditText -> {
                    if (!control.hasFocus() && control.text.toString() != value) {
                        control.setText(value)
                    }
                }
                is Switch -> {
                    val checked = value == "true" || value == "1" || value == "yes"
                    if (control.isChecked != checked) control.isChecked = checked
                }
                is Spinner -> {
                    val idx = def.options.indexOfFirst { it.value == value }.coerceAtLeast(0)
                    if (control.selectedItemPosition != idx) control.setSelection(idx)
                }
            }
            updating[def.key] = false
            resetButtons[def.key]?.visibility =
                if (overrides[def.key]?.isNotEmpty() == true) View.VISIBLE else View.GONE
        }
    }

    // ---------------- 其他 ----------------

    private fun confirmResetAll() {
        MaterialAlertDialogBuilder(this)
            .setTitle(R.string.env_config_reset_all_title)
            .setMessage(R.string.env_config_reset_all_message)
            .setPositiveButton(R.string.env_config_reset_confirm) { _, _ ->
                viewModel.resetAllEnvOverrides()
            }
            .setNegativeButton(R.string.cancel, null)
            .show()
    }

    private fun color(resId: Int): Int = ContextCompat.getColor(this, resId)

    private fun dp(v: Int): Int = (v * resources.displayMetrics.density).toInt()
}
