package com.jianxiang.assistant

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import java.io.File

/**
 * 引擎前台服务：拉起并看护 Go 引擎进程（libjx.so，位于
 * nativeLibraryDir——targetSdk 29+ 下唯一可执行自带二进制的位置），
 * 锁屏/挂后台继续运行（dataSync 型 + START_STICKY）。
 * 通知栏常驻，带「停止」动作；引擎 HTTP 就绪探测通过后广播给 Activity。
 */
class EngineService : Service() {

    companion object {
        const val PORT = 3080
        const val CHANNEL_ID = "engine"
        const val NOTIFY_ID = 1
        const val ACTION_STOP = "com.jianxiang.assistant.STOP_ENGINE"
        private const val TAG = "JXEngine"
        const val BROADCAST_READY = "com.jianxiang.assistant.ENGINE_READY"

        /** 引擎数据根目录（config/data/技能全在这下面，随应用卸载清除）。 */
        fun engineRoot(ctx: Context): File = File(ctx.filesDir, "engine")

        fun libPath(ctx: Context): File {
            // jniLibs 里以 libjx.so 命名，系统释放到 nativeLibraryDir
            return File(ctx.applicationInfo.nativeLibraryDir, "libjx.so")
        }
    }

    private var proc: Process? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        startForegroundWithType()
        // 设备桥：长轮询引擎拿 device_* 命令（无障碍服务执行体）；引擎没起时
        // 桥内静默重试，起了就自动接上（幂等，重复 start 无副作用）
        DeviceBridge.start()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopEngine()
            stopSelf()
            return START_NOT_STICKY
        }
        startEngineIfNeeded()
        return START_STICKY
    }

    private fun startForegroundWithType() {
        val nm = getSystemService(NOTIFICATION_SERVICE) as NotificationManager
        if (Build.VERSION.SDK_INT >= 26) {
            nm.createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "数字助理引擎", NotificationManager.IMPORTANCE_LOW)
            )
        }
        val pi = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val stopPi = PendingIntent.getService(
            this, 1, Intent(this, EngineService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val n = if (Build.VERSION.SDK_INT >= 26) {
            Notification.Builder(this, CHANNEL_ID)
                .setContentTitle("数字助理运行中")
                .setContentText("智能体引擎已在本地就绪，点按打开")
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentIntent(pi)
                .setOngoing(true)
                .addAction(0, "停止", stopPi)
                .build()
        } else {
            @Suppress("DEPRECATION")
            Notification.Builder(this)
                .setContentTitle("数字助理运行中")
                .setContentText("智能体引擎已在本地就绪，点按打开")
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentIntent(pi)
                .setOngoing(true)
                .build()
        }
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFY_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            startForeground(NOTIFY_ID, n)
        }
    }

    /**
     * 引擎进程 + 看护线程：进程死亡后自动重启（服务活着就保引擎活着）。
     * 就绪探测不在这里做——广播发过即焚，热启动的新 Activity 永远收不到；
     * Activity 直连 HTTP 探测（见 MainActivity.probeUntilReady）。
     */
    private fun startEngineIfNeeded() {
        if (proc?.isAlive == true) return
        // 运行时快照（72MB 级）拷贝与进程启动放后台线程，避免主线程 IO
        Thread {
            val root = engineRoot(this).apply {
                mkdirs()
                // 数据根只归本应用 uid，权限最小化
                setWritable(true, false)
            }
            try {
                copyRuntimeAssets(root)
            } catch (e: Exception) {
                Log.e(TAG, "copyRuntimeAssets 失败（引擎将回落系统 sh）", e)
            }
            startProcess(root)
        }.start()
    }

    /**
     * 首启把内置运行时快照与清单从 APK assets 拷到引擎数据目录（幂等：
     * 清单一致且快照在位时走快速路径，约 1KB 读取）。
     */
    private fun copyRuntimeAssets(root: File) {
        val dataDir = File(root, "data").apply { mkdirs() }
        val manifestName = "runtime-MANIFEST.json"
        val assetManifest = try {
            assets.open("runtime/$manifestName").bufferedReader().readText()
        } catch (e: Exception) {
            Log.w(TAG, "assets 无内置运行时（开发包？）", e)
            return
        }
        val dstManifest = File(dataDir, manifestName)
        val dstTar = File(dataDir, "runtime.tgz")
        if (dstManifest.isFile && dstManifest.readText() == assetManifest &&
            dstTar.isFile && dstTar.length() > 1_000_000
        ) {
            return
        }
        for (name in listOf("runtime.tgz", manifestName)) {
            val tmp = File(dataDir, "$name.tmp")
            assets.open("runtime/$name").use { ins ->
                tmp.outputStream().use { outs -> ins.copyTo(outs) }
            }
            val dst = File(dataDir, name)
            dst.delete()
            if (!tmp.renameTo(dst)) throw RuntimeException("运行时快照落位失败: $name")
        }
        Log.i(TAG, "内置运行时资产就绪: ${dstTar.length()} bytes")
    }

    /** 启动引擎进程与看护线程（原 startEngineIfNeeded 主体）。 */
    private fun startProcess(root: File) {
        killOrphanedEngines()
        val lib = libPath(this)
        Log.e(TAG, "engine lib=${lib.absolutePath} exists=${lib.exists()} canExec=${lib.canExecute()}")
        if (!lib.exists() || !lib.canExecute()) {
            Log.e(TAG, "engine lib 不可执行，放弃")
            return
        }
        // config.json 不预置：首次启动引擎用出厂默认配置（空工作区、**没有任何
        // 模型通道**，2026-11-13 起与桌面版同口径），用户接入公司通道或在设置页
        // 录入提供方后自动落盘（引擎 Config.Save 已 0600）
        val args = listOf(
            lib.absolutePath,
            "-noopen",
            "-port", PORT.toString(),
            "-config", File(root, "config.json").absolutePath
        )
        // 收养兜底：极端情况下（如旧版本未带 PDEATHSIG）端口已被某个
        // 幸存引擎占着——与其让新引擎绑定失败进入秒退循环，不如直接
        // 收养现役实例（数据目录同属本应用，服务的是同一份数据）。
        // 新版本引擎均带 PDEATHSIG，孤儿不会再出现，此分支仅防旧残留。
        if (portResponds()) {
            Log.w(TAG, "端口已被幸存引擎占用，收养之（不再重复拉起）")
            return
        }
        try {
            val pb = ProcessBuilder(args).apply {
                directory(root)
                environment()["HOME"] = root.absolutePath
                // 工作区锚定：引擎配置/数据/运行时留在 root 本体，模型工作区
                // 收进 root/workspace 子目录（引擎侧 DefaultConfig 读取此变量，
                // 并负责旧配置 engineRoot → workspace 的迁移与历史会话改指）。
                // 建目录失败只记日志不拦启动（引擎侧 MkdirAll 会再兜一次）
                val ws = File(root, "workspace")
                if (!ws.exists() && !ws.mkdirs()) {
                    Log.e(TAG, "workspace 目录创建失败: ${ws.absolutePath}")
                }
                environment()["JX_WORKSPACE_DEFAULT"] = ws.absolutePath
                redirectErrorStream(true)
            }
            proc = pb.start()
            Log.i(TAG, "engine 进程已启动 pid=${proc.hashCode()}")
            // 读干引擎 stdout（避免管道写满阻塞引擎）；退出即触发守护重启
            val p = proc!!
            Thread {
                val out = try {
                    p.inputStream.use { String(it.readBytes()) }
                } catch (e: Exception) { "<读取失败: ${e.message}>" }
                // exitValue 仅在进程退出后合法；读取异常（流提前关闭等）时进程可能
                // 仍在运行，直接调用会抛 IllegalThreadStateException 拖垮整个应用
                val code = try { p.exitValue() } catch (e: Exception) { -1 }
                Log.e(TAG, "engine 退出 code=$code 输出: ${out.take(4000)}")
                if (proc === p && p.isAlive.not()) {
                    // 非正常退出且服务仍在：1 秒后拉起
                    Thread.sleep(1000)
                    if (proc === p) {
                        proc = null
                        startEngineIfNeeded()
                    }
                }
            }.start()
        } catch (e: Exception) {
            Log.e(TAG, "engine start 失败", e)
            return
        }
    }

    /**
     * 清理孤儿引擎：应用进程崩溃/被杀/重装更新时，它拉起的引擎子进程
     * 会变成孤儿继续运行并占着端口——新引擎绑定失败→秒退→守护重启，
     * 永远循环，启动页永远转圈。拉起前先杀掉同 uid 的遗留 libjx 进程。
     */
    private fun killOrphanedEngines() {
        val mine = android.os.Process.myUid()
        File("/proc").listFiles()?.forEach { d ->
            val pid = d.name.toIntOrNull() ?: return@forEach
            if (pid == android.os.Process.myPid()) return@forEach
            val cmd = runCatching {
                File(d, "cmdline").readBytes().toString(Charsets.UTF_8)
            }.getOrDefault("")
            if (cmd.contains("libjx")) {
                val uid = runCatching {
                    File(d, "status").readLines().firstOrNull { it.startsWith("Uid:") }
                        ?.split(Regex("\\s+"))?.getOrNull(2)?.toInt()
                }.getOrNull()
                if (uid == mine) {
                    Log.w(TAG, "清理孤儿引擎 pid=$pid")
                    runCatching { android.os.Process.killProcess(pid) }
                }
            }
        }
        // 给内核一点时间回收端口
        runCatching { Thread.sleep(200) }
    }

    private fun portResponds(): Boolean = try {
        val c = java.net.URL("http://127.0.0.1:$PORT/api/status").openConnection() as java.net.HttpURLConnection
        c.connectTimeout = 500
        c.readTimeout = 500
        val ok = c.responseCode == 200
        c.disconnect()
        ok
    } catch (e: Exception) { false }

    private fun stopEngine() {
        Log.e(TAG, "stopEngine called")
        proc?.destroy()
        proc = null
    }

    override fun onDestroy() {
        stopEngine()
        super.onDestroy()
    }
}
