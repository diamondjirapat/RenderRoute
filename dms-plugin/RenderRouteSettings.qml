import QtQuick
import qs.Common
import qs.DCommon.Widgets
import qs.Modules.Plugins

PluginSettings {
    id: root
    pluginId: RenderRouteService.pluginId

    StyledText {
        width: parent.width
        text: "RenderRoute Settings"
        font.pixelSize: Theme.fontSizeLarge
        font.weight: Font.Bold
        color: Theme.surfaceText
    }

    StyledText {
        width: parent.width
        text: "Configure GPU telemetry polling, DankBar pill presentation, and desktop launcher integration."
        font.pixelSize: Theme.fontSizeSmall
        color: Theme.surfaceVariantText
        wrapMode: Text.WordWrap
    }

    StringSetting {
        settingKey: "binaryPath"
        label: "RenderRoute Binary"
        description: "Executable name or absolute path to the renderroute CLI binary."
        defaultValue: RenderRouteService.defaults.binaryPath
        placeholder: RenderRouteService.defaults.binaryPath
    }

    SelectionSetting {
        settingKey: "barDisplayMode"
        label: "DankBar Pill Display"
        description: "Choose what information to display next to the GPU icon on the bar."
        defaultValue: RenderRouteService.defaults.barDisplayMode
        options: [
            { value: "status", label: "dGPU Wake Status & Count" },
            { value: "count", label: "Active dGPU Process Count" },
            { value: "default", label: "Default GPU Name" },
            { value: "icon", label: "Icon Only" }
        ]
    }

    SliderSetting {
        settingKey: "pollingInterval"
        label: "Refresh Interval"
        description: "How often to poll GPU status and active processes. Never wakes runtime-suspended dGPUs. Set to 0 to disable polling."
        defaultValue: RenderRouteService.defaults.pollingInterval
        minimum: 0
        maximum: 30000
        unit: "ms"
        leftIcon: "sync"
    }

    ToggleSetting {
        settingKey: "showMetrics"
        label: "Show GPU Telemetry in Popout"
        description: "Display driver, temperature, and VRAM usage on GPU cards inside the popout."
        defaultValue: RenderRouteService.defaults.showMetrics
    }

    ToggleSetting {
        settingKey: "autoPatchDesktop"
        label: "Patch .desktop Launchers on Pin"
        description: "When pinning an application to a GPU, automatically create a user-local override in ~/.local/share/applications so launchers respect the GPU choice."
        defaultValue: RenderRouteService.defaults.autoPatchDesktop
    }
}
