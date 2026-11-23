pragma Singleton

import QtQuick
import Quickshell
import Quickshell.Io
import qs.Common
import qs.Services

Item {
    id: root

    readonly property string pluginId: "renderRoute"
    readonly property string repoUrl: PluginService.availablePlugins[pluginId]?.repo || "https://github.com/diamondjirapat/RenderRoute"

    readonly property var defaults: ({
        binaryPath: "renderroute",
        pollingInterval: 3000,
        barDisplayMode: "status",
        showMetrics: true,
        autoPatchDesktop: true
    })

    property string binaryPath: defaults.binaryPath
    property int pollingInterval: defaults.pollingInterval
    property string barDisplayMode: defaults.barDisplayMode
    property bool showMetrics: defaults.showMetrics
    property bool autoPatchDesktop: defaults.autoPatchDesktop

    property bool available: false
    property bool loading: false
    property bool appsLoaded: false
    property string lastError: ""

    property string defaultGpu: "igpu"
    property string defaultGpuId: ""
    property string defaultGpuShort: "iGPU"
    property bool dgpuActive: false
    property int dgpuProcessCount: 0
    property int totalProcesses: 0
    property var gpus: []
    property var warnings: []
    property var processes: []
    property var rules: []
    property var apps: []

    function loadSettings() {
        const load = (key, fallback) => {
            const v = PluginService.loadPluginData(pluginId, key);
            return (v === undefined || v === null || v === "") ? fallback : v;
        };
        binaryPath = String(load("binaryPath", defaults.binaryPath));
        const pollVal = PluginService.loadPluginData(pluginId, "pollingInterval");
        pollingInterval = (pollVal !== undefined && pollVal !== null) ? Number(pollVal) : defaults.pollingInterval;
        barDisplayMode = String(load("barDisplayMode", defaults.barDisplayMode));
        const metricsVal = PluginService.loadPluginData(pluginId, "showMetrics");
        showMetrics = (metricsVal !== undefined && metricsVal !== null) ? Boolean(metricsVal) : defaults.showMetrics;
        const patchVal = PluginService.loadPluginData(pluginId, "autoPatchDesktop");
        autoPatchDesktop = (patchVal !== undefined && patchVal !== null) ? Boolean(patchVal) : defaults.autoPatchDesktop;

        refresh();
    }

    function ensureRepoMetadata() {
        if (!DMSService || !repoUrl)
            return;
        const list = Array.isArray(DMSService.installedPlugins) ? DMSService.installedPlugins : [];
        const idx = list.findIndex(entry => entry && entry.id === root.pluginId);
        if (idx >= 0) {
            if (list[idx].repo)
                return;
            const updated = list.slice();
            updated[idx] = Object.assign({}, updated[idx], { repo: root.repoUrl });
            DMSService.installedPlugins = updated;
            return;
        }
        DMSService.installedPlugins = list.concat([{
            id: root.pluginId,
            name: "RenderRoute",
            repo: root.repoUrl,
            hasUpdate: false
        }]);
    }

    Component.onCompleted: {
        loadSettings();
        ensureRepoMetadata();
    }

    Connections {
        target: PluginService
        function onPluginDataChanged(changedId) {
            if (changedId === root.pluginId) {
                loadSettings();
            }
        }
    }

    Connections {
        target: DMSService
        function onInstalledPluginsReceived() {
            root.ensureRepoMetadata();
        }
    }

    Timer {
        id: pollTimer
        interval: Math.max(1000, root.pollingInterval)
        running: root.pollingInterval > 0
        repeat: true
        onTriggered: root.refresh()
    }

    function syncGlobals() {
        PluginService.setGlobalVar(pluginId, "available", available);
        PluginService.setGlobalVar(pluginId, "dgpuActive", dgpuActive);
        PluginService.setGlobalVar(pluginId, "dgpuProcessCount", dgpuProcessCount);
        PluginService.setGlobalVar(pluginId, "defaultGpu", defaultGpu);
        PluginService.setGlobalVar(pluginId, "defaultGpuShort", defaultGpuShort);
        PluginService.setGlobalVar(pluginId, "gpus", gpus);
        PluginService.setGlobalVar(pluginId, "warnings", warnings);
    }

    function refresh() {
        if (loading)
            return;
        loading = true;
        Proc.runCommand(`${pluginId}.status`, [binaryPath, "status", "--json"], (stdout, exitCode) => {
            root.loading = false;
            if (exitCode !== 0 || !stdout || stdout.trim().length === 0) {
                root.available = false;
                root.lastError = `Failed to run '${root.binaryPath} status --json' (exit ${exitCode})`;
                root.syncGlobals();
                return;
            }
            try {
                const data = JSON.parse(stdout);
                root.available = true;
                root.lastError = "";
                root.defaultGpu = data.default_gpu || "igpu";
                root.defaultGpuId = data.default_gpu_id || "";
                root.defaultGpuShort = data.default_gpu_short || root.defaultGpu;
                root.dgpuActive = Boolean(data.dgpu_active);
                root.dgpuProcessCount = Number(data.dgpu_process_count || 0);
                root.totalProcesses = Number(data.total_processes || 0);
                root.gpus = Array.isArray(data.gpus) ? data.gpus : [];
                root.warnings = Array.isArray(data.warnings) ? data.warnings : [];
                root.processes = Array.isArray(data.processes) ? data.processes : [];
                root.rules = Array.isArray(data.rules) ? data.rules : [];
                root.syncGlobals();
            } catch (e) {
                root.available = false;
                root.lastError = "Failed to parse RenderRoute JSON: " + e;
                root.syncGlobals();
            }
        }, 50, 5000);
    }

    function fetchApps(query) {
        const args = [binaryPath, "apps", "--json"];
        if (query && query.trim().length > 0) {
            args.push("--search", query.trim());
        }
        Proc.runCommand(`${pluginId}.apps`, args, (stdout, exitCode) => {
            if (exitCode !== 0 || !stdout)
                return;
            try {
                const parsed = JSON.parse(stdout);
                root.apps = Array.isArray(parsed) ? parsed : [];
                root.appsLoaded = true;
            } catch (e) {
                console.warn("RenderRoute: failed to parse apps:", e);
            }
        }, 50, 5000);
    }

    function selectorForGpu(g) {
        if (!g)
            return "igpu";
        if (g.role === "igpu" || g.role === "dgpu")
            return g.role;
        return g.id || String(g.index);
    }

    function setDefaultGpu(gpuSelector) {
        Proc.runCommand(`${pluginId}.setDefault`, [binaryPath, "default", gpuSelector], (stdout, exitCode) => {
            if (exitCode === 0) {
                ToastService.showInfo("Default GPU set to " + gpuSelector.toUpperCase());
                root.refresh();
            } else {
                ToastService.showError("Failed to set default GPU");
            }
        }, 50, 5000);
    }

    function cycleDefaultGpu() {
        if (!gpus || gpus.length < 2) {
            refresh();
            return;
        }
        let idx = gpus.findIndex(g => g.id === defaultGpuId || g.role === defaultGpu);
        const next = gpus[(idx + 1) % gpus.length];
        setDefaultGpu(selectorForGpu(next));
    }

    function setRule(target, gpuSelector, desktopId, displayName, applyDesktop) {
        if (!target || target.trim().length === 0)
            return;
        const args = [binaryPath, "rule", "set", target.trim(), gpuSelector];
        if (desktopId && desktopId.length > 0) {
            args.push("--desktop-id", desktopId);
        }
        if (displayName && displayName.length > 0) {
            args.push("--name", displayName);
        }
        if (applyDesktop) {
            args.push("--apply-desktop");
        }
        Proc.runCommand(`${pluginId}.setRule`, args, (stdout, exitCode) => {
            if (exitCode === 0) {
                ToastService.showInfo(`Pinned ${displayName || target} -> ${gpuSelector.toUpperCase()}`);
                root.refresh();
                if (root.appsLoaded)
                    root.fetchApps("");
            } else {
                ToastService.showError(`Failed to save rule for ${target}`);
            }
        }, 50, 5000);
    }

    function removeRule(target, desktopId) {
        if (!target && !desktopId)
            return;
        const args = [binaryPath, "rule", "remove"];
        if (target && target.length > 0) {
            args.push(target);
        }
        if (desktopId && desktopId.length > 0) {
            args.push("--desktop-id", desktopId);
        }
        Proc.runCommand(`${pluginId}.rmRule`, args, (stdout, exitCode) => {
            if (exitCode === 0) {
                ToastService.showInfo(`Removed GPU rule for ${target || desktopId}`);
                root.refresh();
                if (root.appsLoaded)
                    root.fetchApps("");
            } else {
                ToastService.showError(`Failed to remove rule for ${target || desktopId}`);
            }
        }, 50, 5000);
    }

    function launchApp(appId, gpuSelector, appName) {
        const args = [binaryPath, "launch", appId];
        if (gpuSelector && gpuSelector.length > 0) {
            args.push("--gpu", gpuSelector);
        }
        Proc.runCommand(`${pluginId}.launch`, args, (stdout, exitCode) => {
            if (exitCode === 0) {
                ToastService.showInfo(`Launched ${appName || appId} on ${(gpuSelector || defaultGpu).toUpperCase()}`);
                Qt.callLater(() => root.refresh());
            } else {
                ToastService.showError(`Failed to launch ${appName || appId}`);
            }
        }, 50, 5000);
    }

    function runCommandOnGpu(commandText, gpuSelector) {
        const trimmed = (commandText || "").trim();
        if (trimmed.length === 0)
            return;
        const sel = gpuSelector || defaultGpu || "igpu";
        Proc.runCommand(`${pluginId}.runCmd`, [binaryPath, "run", sel, "--detach", "--", "sh", "-c", trimmed], (stdout, exitCode) => {
            if (exitCode === 0) {
                ToastService.showInfo(`Launched command on ${sel.toUpperCase()}`);
                Qt.callLater(() => root.refresh());
            } else {
                ToastService.showError("Failed to launch command on " + sel);
            }
        }, 50, 5000);
    }

    function relaunchProcess(pid, procName, gpuSelector) {
        Proc.runCommand(`${pluginId}.relaunch`, [binaryPath, "relaunch", String(pid), gpuSelector], (stdout, exitCode) => {
            if (exitCode === 0) {
                ToastService.showInfo(`Relaunched ${procName} on ${gpuSelector.toUpperCase()}`);
                Qt.callLater(() => root.refresh());
            } else {
                ToastService.showError(`Could not relaunch ${procName} (PID ${pid})`);
            }
        }, 50, 8000);
    }
}
