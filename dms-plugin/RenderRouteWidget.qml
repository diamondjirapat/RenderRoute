import QtQuick
import Quickshell
import qs.Common
import qs.Services
import qs.DCommon.Widgets
import qs.Modules.ControlCenter
import qs.Modules.ControlCenter.Widgets
import qs.Modules.Plugins

PluginComponent {
    id: root

    property int activeTab: 0 // 0 = Processes, 1 = Apps & Run, 2 = Rules
    property string processFilter: "all" // "all", "dgpu", "igpu"
    property string expandedProcessKey: ""
    property string appSearchQuery: ""
    property string quickRunGpu: "dgpu"
    property string newRuleGpu: "dgpu"

    Component.onCompleted: {
        // Ensure singleton initializes immediately
        RenderRouteService.refresh();
    }

    readonly property string barLabelText: {
        if (!RenderRouteService.available)
            return "ERR";
        const mode = RenderRouteService.barDisplayMode || "status";
        if (mode === "icon")
            return "";
        if (mode === "count")
            return RenderRouteService.dgpuProcessCount > 0 ? String(RenderRouteService.dgpuProcessCount) : "";
        if (mode === "default")
            return RenderRouteService.defaultGpuShort || RenderRouteService.defaultGpu.toUpperCase();
        // Default "status" mode
        if (RenderRouteService.dgpuActive) {
            return RenderRouteService.dgpuProcessCount > 0
                ? `dGPU ${RenderRouteService.dgpuProcessCount}`
                : "dGPU";
        }
        return "iGPU";
    }

    readonly property color barIconColor: {
        if (!RenderRouteService.available)
            return Theme.error;
        if (RenderRouteService.dgpuActive)
            return Theme.primary;
        return Theme.widgetIconColor || Theme.surfaceText;
    }

    readonly property var filteredProcesses: {
        const list = RenderRouteService.processes || [];
        if (processFilter === "all")
            return list;
        return list.filter(p => p.gpu_role === processFilter);
    }

    readonly property var filteredApps: {
        const list = RenderRouteService.apps || [];
        const q = (appSearchQuery || "").trim().toLowerCase();
        if (q.length === 0)
            return list.slice(0, 60);
        return list.filter(a => {
            const hay = ((a.name || "") + " " + (a.id || "") + " " + (a.binary || "")).toLowerCase();
            return hay.indexOf(q) !== -1;
        }).slice(0, 60);
    }

    // --- Control Center Tile Integration ---
    ccWidgetIcon: "memory"
    ccWidgetPrimaryText: "RenderRoute"
    ccWidgetSecondaryText: {
        if (!RenderRouteService.available)
            return "CLI not found";
        if (RenderRouteService.dgpuActive)
            return `dGPU active · ${RenderRouteService.dgpuProcessCount} proc`;
        return `Default: ${RenderRouteService.defaultGpuShort}`;
    }
    ccWidgetIsActive: RenderRouteService.available && RenderRouteService.dgpuActive
    onCcWidgetToggled: RenderRouteService.cycleDefaultGpu()

    ccExpandedContent: Component {
        CcTileActions {
            actions: (RenderRouteService.gpus || []).map(g => ({
                text: `${g.short_name || g.card} (${(g.role || "").toUpperCase()})`,
                icon: g.role === "dgpu" ? "bolt" : "eco",
                active: g.id === RenderRouteService.defaultGpuId || g.role === RenderRouteService.defaultGpu,
                trigger: () => RenderRouteService.setDefaultGpu(RenderRouteService.selectorForGpu(g))
            })).concat([{
                text: "Refresh GPUs",
                icon: "refresh",
                trigger: () => RenderRouteService.refresh()
            }])
        }
    }

    ccDetailContent: Component {
        Item {
            readonly property string title: "RenderRoute GPUs"

            DFlickable {
                anchors.fill: parent
                contentHeight: ccCol.height
                clip: true

                Column {
                    id: ccCol
                    width: parent.width
                    spacing: CcMetrics.detailContentGap

                    CcEmptyState {
                        visible: !RenderRouteService.available
                        iconName: "error"
                        title: "RenderRoute binary not found. Install 'renderroute' into PATH."
                    }

                    CcGroup {
                        visible: RenderRouteService.gpus.length > 0

                        Repeater {
                            model: RenderRouteService.gpus

                            CcListRow {
                                required property var modelData
                                iconName: modelData.role === "dgpu" ? "bolt" : "eco"
                                title: modelData.name || modelData.card
                                subtitle: {
                                    let parts = [(modelData.role || "").toUpperCase(), modelData.power_state || "unknown"];
                                    if (modelData.active_proc_count > 0)
                                        parts.push(`${modelData.active_proc_count} proc`);
                                    if (modelData.temperature_c !== undefined && modelData.temperature_c !== null)
                                        parts.push(`${Math.round(modelData.temperature_c)}°C`);
                                    return parts.join(" · ");
                                }
                                active: modelData.id === RenderRouteService.defaultGpuId || modelData.role === RenderRouteService.defaultGpu
                                clickable: true
                                onClicked: RenderRouteService.setDefaultGpu(RenderRouteService.selectorForGpu(modelData))
                            }
                        }
                    }
                }
            }
        }
    }

    // --- DankBar Horizontal & Vertical Pills ---
    pillRightClickAction: function() {
        RenderRouteService.refresh();
        ToastService.showInfo("Refreshed RenderRoute GPU status");
    }

    horizontalBarPill: Component {
        Row {
            spacing: Theme.spacingXS

            DIcon {
                name: "memory"
                size: root.iconSize
                color: root.barIconColor
                anchors.verticalCenter: parent.verticalCenter
            }

            StyledText {
                visible: root.barLabelText.length > 0
                text: root.barLabelText
                font.pixelSize: Theme.barTextSize(root.barThickness, root.barConfig?.fontScale)
                color: Theme.widgetTextColor || Theme.surfaceText
                anchors.verticalCenter: parent.verticalCenter
            }
        }
    }

    verticalBarPill: Component {
        Column {
            spacing: Theme.spacingXXS

            DIcon {
                name: "memory"
                size: root.iconSize
                color: root.barIconColor
                anchors.horizontalCenter: parent.horizontalCenter
            }

            StyledText {
                visible: RenderRouteService.dgpuProcessCount > 0
                text: String(RenderRouteService.dgpuProcessCount)
                font.pixelSize: Theme.barTextSize(root.barThickness, root.barConfig?.fontScale)
                color: Theme.widgetTextColor || Theme.surfaceText
                anchors.horizontalCenter: parent.horizontalCenter
            }
        }
    }

    // --- Reusable Inline Components ---
    component TabPill: Rectangle {
        id: tabPillRoot
        property string label: ""
        property bool active: false
        signal clicked

        height: 30
        radius: Theme.cornerRadius
        color: active ? Theme.primary : (tabMouse.containsMouse ? Theme.surfaceHover : Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency))

        StyledText {
            anchors.centerIn: parent
            text: tabPillRoot.label
            font.pixelSize: Theme.fontSizeSmall
            font.weight: tabPillRoot.active ? Font.DemiBold : Font.Normal
            color: tabPillRoot.active ? Theme.primaryText : Theme.surfaceText
        }

        MouseArea {
            id: tabMouse
            anchors.fill: parent
            hoverEnabled: true
            cursorShape: Qt.PointingHandCursor
            onClicked: tabPillRoot.clicked()
        }
    }

    component MiniChipButton: Rectangle {
        id: chipRoot
        property string label: ""
        property string iconName: ""
        property bool highlighted: false
        property bool danger: false
        signal clicked

        implicitWidth: chipRow.implicitWidth + Theme.spacingM
        height: 26
        radius: Theme.cornerRadius
        color: {
            if (danger)
                return chipMouse.containsMouse ? Theme.error : Theme.withAlpha(Theme.error, 0.16);
            if (highlighted)
                return Theme.primary;
            return chipMouse.containsMouse ? Theme.surfaceHover : Theme.withAlpha(Theme.surfaceContainerHighest, Theme.popupTransparency);
        }
        border.width: highlighted ? 0 : 1
        border.color: danger ? Theme.withAlpha(Theme.error, 0.4) : Theme.outlineLight

        Row {
            id: chipRow
            anchors.centerIn: parent
            spacing: 4

            DIcon {
                visible: chipRoot.iconName.length > 0
                name: chipRoot.iconName
                size: 13
                color: {
                    if (chipRoot.danger)
                        return chipMouse.containsMouse ? Theme.surface : Theme.error;
                    if (chipRoot.highlighted)
                        return Theme.primaryText;
                    return Theme.surfaceText;
                }
                anchors.verticalCenter: parent.verticalCenter
            }

            StyledText {
                visible: chipRoot.label.length > 0
                text: chipRoot.label
                font.pixelSize: Theme.fontSizeSmall - 1
                font.weight: chipRoot.highlighted ? Font.DemiBold : Font.Medium
                color: {
                    if (chipRoot.danger)
                        return chipMouse.containsMouse ? Theme.surface : Theme.error;
                    if (chipRoot.highlighted)
                        return Theme.primaryText;
                    return Theme.surfaceText;
                }
                anchors.verticalCenter: parent.verticalCenter
            }
        }

        MouseArea {
            id: chipMouse
            anchors.fill: parent
            hoverEnabled: true
            cursorShape: Qt.PointingHandCursor
            onClicked: chipRoot.clicked()
        }
    }

    // --- Popout Panel ---
    popoutWidth: 450
    popoutHeight: 560

    popoutContent: Component {
        FocusScope {
            id: popoutScope
            implicitWidth: 450
            implicitHeight: 560
            focus: true

            property var parentPopout: null
            Connections {
                target: parentPopout
                function onOpened() {
                    RenderRouteService.refresh();
                    if (root.activeTab === 1 && !RenderRouteService.appsLoaded) {
                        RenderRouteService.fetchApps("");
                    }
                }
            }

            Column {
                anchors.fill: parent
                anchors.margins: Theme.spacingM
                spacing: Theme.spacingS

                // 1. Header
                Item {
                    width: parent.width
                    height: 36

                    Row {
                        anchors.left: parent.left
                        anchors.verticalCenter: parent.verticalCenter
                        spacing: Theme.spacingS

                        DIcon {
                            name: "memory"
                            size: Theme.iconSize + 2
                            color: RenderRouteService.dgpuActive ? Theme.primary : Theme.surfaceText
                            anchors.verticalCenter: parent.verticalCenter
                        }

                        Column {
                            anchors.verticalCenter: parent.verticalCenter
                            spacing: 1

                            StyledText {
                                text: "RenderRoute"
                                font.pixelSize: Theme.fontSizeMedium
                                font.weight: Font.Bold
                                color: Theme.surfaceText
                            }

                            StyledText {
                                text: RenderRouteService.available
                                    ? `Default: ${RenderRouteService.defaultGpuShort} · ${RenderRouteService.totalProcesses} active`
                                    : "Backend binary unavailable"
                                font.pixelSize: Theme.fontSizeSmall - 1
                                color: Theme.surfaceVariantText
                            }
                        }
                    }

                    Row {
                        anchors.right: parent.right
                        anchors.verticalCenter: parent.verticalCenter
                        spacing: Theme.spacingXS

                        Rectangle {
                            visible: RenderRouteService.available
                            height: 24
                            width: dgpuPillRow.implicitWidth + 14
                            radius: 12
                            anchors.verticalCenter: parent.verticalCenter
                            color: RenderRouteService.dgpuActive
                                ? Theme.withAlpha(Theme.primary, 0.18)
                                : Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency)
                            border.width: 1
                            border.color: RenderRouteService.dgpuActive
                                ? Theme.withAlpha(Theme.primary, 0.45)
                                : Theme.outlineLight

                            Row {
                                id: dgpuPillRow
                                anchors.centerIn: parent
                                spacing: 5

                                Rectangle {
                                    width: 7
                                    height: 7
                                    radius: 4
                                    anchors.verticalCenter: parent.verticalCenter
                                    color: RenderRouteService.dgpuActive ? Theme.primary : Theme.surfaceVariantText
                                }

                                StyledText {
                                    text: RenderRouteService.dgpuActive ? "dGPU Awake" : "dGPU Asleep"
                                    font.pixelSize: Theme.fontSizeSmall - 1
                                    font.weight: Font.Medium
                                    color: RenderRouteService.dgpuActive ? Theme.primary : Theme.surfaceVariantText
                                    anchors.verticalCenter: parent.verticalCenter
                                }
                            }
                        }

                        Rectangle {
                            width: 30
                            height: 30
                            radius: Theme.cornerRadius
                            color: refreshMouse.containsMouse ? Theme.surfaceHover : "transparent"

                            DIcon {
                                anchors.centerIn: parent
                                name: "refresh"
                                size: 18
                                color: Theme.surfaceText
                            }

                            MouseArea {
                                id: refreshMouse
                                anchors.fill: parent
                                hoverEnabled: true
                                cursorShape: Qt.PointingHandCursor
                                onClicked: {
                                    RenderRouteService.refresh();
                                    if (root.activeTab === 1)
                                        RenderRouteService.fetchApps("");
                                }
                            }
                        }
                    }
                }

                // Missing CLI fallback banner
                StyledRect {
                    visible: !RenderRouteService.available
                    width: parent.width
                    height: missingCol.implicitHeight + Theme.spacingM * 2
                    radius: Theme.cornerRadius
                    color: Theme.withAlpha(Theme.error, 0.12)
                    border.width: 1
                    border.color: Theme.withAlpha(Theme.error, 0.4)

                    Column {
                        id: missingCol
                        anchors.fill: parent
                        anchors.margins: Theme.spacingM
                        spacing: Theme.spacingXS

                        StyledText {
                            text: "renderroute CLI not found"
                            font.pixelSize: Theme.fontSizeMedium
                            font.weight: Font.Bold
                            color: Theme.error
                        }

                        StyledText {
                            width: parent.width
                            wrapMode: Text.WordWrap
                            text: RenderRouteService.lastError || "Install the renderroute binary into ~/.local/bin or set its path in plugin settings."
                            font.pixelSize: Theme.fontSizeSmall
                            color: Theme.surfaceText
                        }
                    }
                }

                // Uninitialized PCI GPU warning banner
                StyledRect {
                    id: gpuWarningBanner
                    visible: RenderRouteService.available && RenderRouteService.warnings.length > 0
                    width: parent.width
                    height: warningCol.implicitHeight + Theme.spacingS * 2
                    radius: Theme.cornerRadius
                    color: Theme.withAlpha(Theme.warning, 0.14)
                    border.width: 1
                    border.color: Theme.withAlpha(Theme.warning, 0.45)

                    Column {
                        id: warningCol
                        anchors.left: parent.left
                        anchors.right: parent.right
                        anchors.top: parent.top
                        anchors.margins: Theme.spacingS
                        spacing: 3

                        Repeater {
                            model: RenderRouteService.warnings

                            Column {
                                required property var modelData
                                width: warningCol.width
                                spacing: 1

                                StyledText {
                                    width: parent.width
                                    text: `GPU driver issue: ${modelData.name || modelData.pci_slot} (${(modelData.role || "").toUpperCase()})`
                                    font.pixelSize: Theme.fontSizeSmall
                                    font.weight: Font.Bold
                                    color: Theme.warning
                                    elide: Text.ElideRight
                                }

                                StyledText {
                                    width: parent.width
                                    wrapMode: Text.WordWrap
                                    text: modelData.reason || "No DRM device in /sys/class/drm"
                                    font.pixelSize: Theme.fontSizeSmall - 2
                                    color: Theme.surfaceText
                                }
                            }
                        }
                    }
                }

                // 2. GPU Cards Strip (click a card to set Default GPU)
                Row {
                    id: gpuCardsRow
                    visible: RenderRouteService.gpus.length > 0
                    width: parent.width
                    spacing: Theme.spacingS

                    Repeater {
                        model: RenderRouteService.gpus

                        StyledRect {
                            required property var modelData
                            readonly property bool isDefault: modelData.id === RenderRouteService.defaultGpuId || modelData.role === RenderRouteService.defaultGpu
                            readonly property bool isSleeping: modelData.power_state === "suspended"

                            width: (gpuCardsRow.width - Theme.spacingS * Math.max(0, RenderRouteService.gpus.length - 1)) / Math.max(1, RenderRouteService.gpus.length)
                            height: gpuCardCol.implicitHeight + Theme.spacingS * 2
                            radius: Theme.cornerRadius
                            color: gpuCardMouse.containsMouse
                                ? Theme.withAlpha(Theme.surfaceContainerHighest, Theme.popupTransparency)
                                : Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency)
                            border.width: isDefault ? 1.5 : 1
                            border.color: isDefault ? Theme.primary : Theme.outlineLight

                            Column {
                                id: gpuCardCol
                                anchors.left: parent.left
                                anchors.right: parent.right
                                anchors.top: parent.top
                                anchors.margins: Theme.spacingS
                                spacing: 3

                                Item {
                                    width: parent.width
                                    height: 18

                                    Row {
                                        anchors.left: parent.left
                                        anchors.verticalCenter: parent.verticalCenter
                                        spacing: 4

                                        DIcon {
                                            name: modelData.role === "dgpu" ? "bolt" : "eco"
                                            size: 14
                                            color: isDefault ? Theme.primary : Theme.surfaceVariantText
                                            anchors.verticalCenter: parent.verticalCenter
                                        }

                                        StyledText {
                                            text: (modelData.role || modelData.card).toUpperCase()
                                            font.pixelSize: Theme.fontSizeSmall - 1
                                            font.weight: Font.Bold
                                            color: isDefault ? Theme.primary : Theme.surfaceVariantText
                                            anchors.verticalCenter: parent.verticalCenter
                                        }
                                    }

                                    StyledText {
                                        anchors.right: parent.right
                                        anchors.verticalCenter: parent.verticalCenter
                                        text: isDefault ? "DEFAULT" : (isSleeping ? "SLEEP" : (modelData.active_proc_count + " proc"))
                                        font.pixelSize: Theme.fontSizeSmall - 2
                                        font.weight: Font.DemiBold
                                        color: isDefault ? Theme.primary : (isSleeping ? Theme.surfaceVariantText : Theme.surfaceText)
                                    }
                                }

                                StyledText {
                                    width: parent.width
                                    text: modelData.short_name || modelData.name
                                    font.pixelSize: Theme.fontSizeSmall
                                    font.weight: Font.Bold
                                    color: Theme.surfaceText
                                    elide: Text.ElideRight
                                }

                                StyledText {
                                    visible: RenderRouteService.showMetrics
                                    width: parent.width
                                    font.pixelSize: Theme.fontSizeSmall - 2
                                    color: Theme.surfaceVariantText
                                    elide: Text.ElideRight
                                    text: {
                                        let parts = [modelData.driver || modelData.card];
                                        if (isSleeping) {
                                            parts.push("suspended");
                                        } else {
                                            if (modelData.temperature_c !== undefined && modelData.temperature_c !== null)
                                                parts.push(`${Math.round(modelData.temperature_c)}°C`);
                                            if (modelData.vram_used_mb !== undefined && modelData.vram_total_mb !== undefined)
                                                parts.push(`${Math.round(modelData.vram_used_mb)}/${Math.round(modelData.vram_total_mb)}M`);
                                            else if (modelData.active_proc_count > 0)
                                                parts.push(`${modelData.active_proc_count} proc`);
                                        }
                                        return parts.join(" · ");
                                    }
                                }
                            }

                            MouseArea {
                                id: gpuCardMouse
                                anchors.fill: parent
                                hoverEnabled: true
                                cursorShape: Qt.PointingHandCursor
                                onClicked: RenderRouteService.setDefaultGpu(RenderRouteService.selectorForGpu(modelData))
                            }
                        }
                    }
                }

                // 3. Tab Bar
                Row {
                    id: tabRow
                    width: parent.width
                    spacing: Theme.spacingXS

                    TabPill {
                        width: (tabRow.width - Theme.spacingXS * 2) / 3
                        label: `Processes (${RenderRouteService.totalProcesses})`
                        active: root.activeTab === 0
                        onClicked: root.activeTab = 0
                    }

                    TabPill {
                        width: (tabRow.width - Theme.spacingXS * 2) / 3
                        label: "Apps & Run"
                        active: root.activeTab === 1
                        onClicked: {
                            root.activeTab = 1;
                            if (!RenderRouteService.appsLoaded)
                                RenderRouteService.fetchApps("");
                        }
                    }

                    TabPill {
                        width: (tabRow.width - Theme.spacingXS * 2) / 3
                        label: `Rules (${RenderRouteService.rules.length})`
                        active: root.activeTab === 2
                        onClicked: root.activeTab = 2
                    }
                }

                // 4. Tab Content Area
                Item {
                    width: parent.width
                    height: Math.max(180, popoutScope.height - Theme.spacingM * 2 - 36 - (gpuWarningBanner.visible ? gpuWarningBanner.height + Theme.spacingS : 0) - (gpuCardsRow.visible ? gpuCardsRow.height + Theme.spacingS : 0) - 30 - Theme.spacingS * 3)

                    // ================= TAB 0: ACTIVE GPU PROCESSES =================
                    Column {
                        visible: root.activeTab === 0
                        anchors.fill: parent
                        spacing: Theme.spacingXS

                        Row {
                            spacing: Theme.spacingXS

                            MiniChipButton {
                                label: `All (${RenderRouteService.totalProcesses})`
                                highlighted: root.processFilter === "all"
                                onClicked: root.processFilter = "all"
                            }

                            MiniChipButton {
                                label: `dGPU (${RenderRouteService.dgpuProcessCount})`
                                iconName: "bolt"
                                highlighted: root.processFilter === "dgpu"
                                onClicked: root.processFilter = "dgpu"
                            }

                            MiniChipButton {
                                label: `iGPU (${Math.max(0, RenderRouteService.totalProcesses - RenderRouteService.dgpuProcessCount)})`
                                iconName: "eco"
                                highlighted: root.processFilter === "igpu"
                                onClicked: root.processFilter = "igpu"
                            }
                        }

                        DFlickable {
                            width: parent.width
                            height: parent.height - 34
                            contentHeight: procListCol.implicitHeight
                            clip: true

                            Column {
                                id: procListCol
                                width: parent.width
                                spacing: Theme.spacingXS

                                StyledText {
                                    visible: root.filteredProcesses.length === 0
                                    width: parent.width
                                    topPadding: Theme.spacingL
                                    horizontalAlignment: Text.AlignHCenter
                                    text: "No active GPU processes in this view."
                                    font.pixelSize: Theme.fontSizeSmall
                                    color: Theme.surfaceVariantText
                                }

                                Repeater {
                                    model: root.filteredProcesses

                                    StyledRect {
                                        id: procCard
                                        required property var modelData
                                        readonly property var procData: modelData
                                        readonly property string rowKey: `${procData.pid}:${procData.gpu_id}`
                                        readonly property bool expanded: root.expandedProcessKey === rowKey
                                        readonly property bool ruleMismatch: Boolean(procData.rule_gpu) && procData.rule_matches === false

                                        width: procListCol.width
                                        height: procCardCol.implicitHeight + Theme.spacingS * 2
                                        radius: Theme.cornerRadius
                                        color: procMouse.containsMouse || expanded
                                            ? Theme.withAlpha(Theme.surfaceContainerHighest, Theme.popupTransparency)
                                            : Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency)
                                        border.width: 1
                                        border.color: ruleMismatch ? Theme.warning : Theme.outlineLight

                                        Column {
                                            id: procCardCol
                                            anchors.left: parent.left
                                            anchors.right: parent.right
                                            anchors.top: parent.top
                                            anchors.margins: Theme.spacingS
                                            spacing: Theme.spacingXS

                                            Item {
                                                width: parent.width
                                                height: 32

                                                Column {
                                                    anchors.left: parent.left
                                                    anchors.right: procBadges.left
                                                    anchors.rightMargin: Theme.spacingS
                                                    anchors.verticalCenter: parent.verticalCenter
                                                    spacing: 1

                                                    StyledText {
                                                        width: parent.width
                                                        text: procCard.procData.name || ("PID " + procCard.procData.pid)
                                                        font.pixelSize: Theme.fontSizeSmall
                                                        font.weight: Font.Bold
                                                        color: Theme.surfaceText
                                                        elide: Text.ElideRight
                                                    }

                                                    StyledText {
                                                        width: parent.width
                                                        text: {
                                                            let info = `PID ${procCard.procData.pid}`;
                                                            if (procCard.procData.vram_used_mb)
                                                                info += ` · ${Math.round(procCard.procData.vram_used_mb)} MB`;
                                                            if (procCard.procData.rule_gpu)
                                                                info += procCard.ruleMismatch ? ` · Rule: ${procCard.procData.rule_gpu} (restart)` : ` · Rule: ${procCard.procData.rule_gpu}`;
                                                            return info;
                                                        }
                                                        font.pixelSize: Theme.fontSizeSmall - 2
                                                        color: procCard.ruleMismatch ? Theme.warning : Theme.surfaceVariantText
                                                        elide: Text.ElideRight
                                                    }
                                                }

                                                Row {
                                                    id: procBadges
                                                    anchors.right: parent.right
                                                    anchors.verticalCenter: parent.verticalCenter
                                                    spacing: Theme.spacingXS

                                                    Rectangle {
                                                        height: 22
                                                        width: gpuTagText.implicitWidth + 12
                                                        radius: 11
                                                        anchors.verticalCenter: parent.verticalCenter
                                                        color: procCard.procData.gpu_role === "dgpu"
                                                            ? Theme.withAlpha(Theme.primary, 0.2)
                                                            : Theme.withAlpha(Theme.surfaceContainerHighest, Theme.popupTransparency)

                                                        StyledText {
                                                            id: gpuTagText
                                                            anchors.centerIn: parent
                                                            text: procCard.procData.gpu_short || procCard.procData.gpu_role
                                                            font.pixelSize: Theme.fontSizeSmall - 2
                                                            font.weight: Font.DemiBold
                                                            color: procCard.procData.gpu_role === "dgpu" ? Theme.primary : Theme.surfaceText
                                                        }
                                                    }

                                                    DIcon {
                                                        name: procCard.expanded ? "expand_less" : "expand_more"
                                                        size: 16
                                                        color: Theme.surfaceVariantText
                                                        anchors.verticalCenter: parent.verticalCenter
                                                    }
                                                }

                                                MouseArea {
                                                    id: procMouse
                                                    anchors.fill: parent
                                                    hoverEnabled: true
                                                    cursorShape: Qt.PointingHandCursor
                                                    onClicked: {
                                                        root.expandedProcessKey = procCard.expanded ? "" : procCard.rowKey;
                                                    }
                                                }
                                            }

                                            // Expanded per-process GPU routing actions
                                            Flow {
                                                visible: procCard.expanded
                                                width: parent.width
                                                spacing: Theme.spacingXS

                                                Repeater {
                                                    model: RenderRouteService.gpus

                                                    MiniChipButton {
                                                        required property var modelData
                                                        readonly property string targetSel: RenderRouteService.selectorForGpu(modelData)
                                                        label: `Pin ${modelData.short_name || targetSel}`
                                                        iconName: "push_pin"
                                                        highlighted: procCard.procData.rule_gpu === targetSel || procCard.procData.rule_gpu === modelData.id
                                                        onClicked: RenderRouteService.setRule(
                                                            procCard.procData.name,
                                                            targetSel,
                                                            procCard.procData.desktop_id || "",
                                                            procCard.procData.name,
                                                            RenderRouteService.autoPatchDesktop
                                                        )
                                                    }
                                                }

                                                Repeater {
                                                    model: RenderRouteService.gpus

                                                    MiniChipButton {
                                                        required property var modelData
                                                        readonly property string targetSel: RenderRouteService.selectorForGpu(modelData)
                                                        visible: Boolean(procCard.procData.owned_by_user) && modelData.id !== procCard.procData.gpu_id
                                                        label: `Relaunch on ${modelData.short_name || targetSel}`
                                                        iconName: "restart_alt"
                                                        onClicked: RenderRouteService.relaunchProcess(
                                                            procCard.procData.pid,
                                                            procCard.procData.name,
                                                            targetSel
                                                        )
                                                    }
                                                }
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }

                    // ================= TAB 1: APPS & QUICK RUN =================
                    Column {
                        visible: root.activeTab === 1
                        anchors.fill: parent
                        spacing: Theme.spacingS

                        // Quick Run Command on Chosen GPU
                        StyledRect {
                            width: parent.width
                            height: quickRunCol.implicitHeight + Theme.spacingS * 2
                            radius: Theme.cornerRadius
                            color: Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency)
                            border.width: 1
                            border.color: Theme.outlineLight

                            Column {
                                id: quickRunCol
                                anchors.left: parent.left
                                anchors.right: parent.right
                                anchors.top: parent.top
                                anchors.margins: Theme.spacingS
                                spacing: Theme.spacingXS

                                StyledText {
                                    text: "Run Command on Specific GPU"
                                    font.pixelSize: Theme.fontSizeSmall
                                    font.weight: Font.DemiBold
                                    color: Theme.surfaceText
                                }

                                Row {
                                    width: parent.width
                                    spacing: Theme.spacingXS

                                    DTextField {
                                        id: quickCmdField
                                        width: parent.width - runBtn.width - Theme.spacingXS
                                        height: 32
                                        placeholderText: "e.g. vkcube, blender, steam..."
                                        onAccepted: {
                                            RenderRouteService.runCommandOnGpu(text, root.quickRunGpu);
                                            text = "";
                                        }
                                    }

                                    MiniChipButton {
                                        id: runBtn
                                        height: 32
                                        label: "Run"
                                        iconName: "play_arrow"
                                        highlighted: true
                                        onClicked: {
                                            RenderRouteService.runCommandOnGpu(quickCmdField.text, root.quickRunGpu);
                                            quickCmdField.text = "";
                                        }
                                    }
                                }

                                Row {
                                    spacing: Theme.spacingXS

                                    Repeater {
                                        model: RenderRouteService.gpus

                                        MiniChipButton {
                                            required property var modelData
                                            readonly property string sel: RenderRouteService.selectorForGpu(modelData)
                                            label: modelData.short_name || sel.toUpperCase()
                                            iconName: modelData.role === "dgpu" ? "bolt" : "eco"
                                            highlighted: root.quickRunGpu === sel
                                            onClicked: root.quickRunGpu = sel
                                        }
                                    }
                                }
                            }
                        }

                        // Installed Applications Search
                        DTextField {
                            id: appSearchField
                            width: parent.width
                            height: 32
                            leftIconName: "search"
                            placeholderText: "Search desktop apps to launch or pin..."
                            showClearButton: true
                            onTextChanged: root.appSearchQuery = text
                        }

                        DFlickable {
                            width: parent.width
                            height: Math.max(80, parent.height - quickRunCol.implicitHeight - Theme.spacingS * 2 - 32 - Theme.spacingS * 2)
                            contentHeight: appsCol.implicitHeight
                            clip: true

                            Column {
                                id: appsCol
                                width: parent.width
                                spacing: Theme.spacingXS

                                StyledText {
                                    visible: root.filteredApps.length === 0
                                    width: parent.width
                                    topPadding: Theme.spacingM
                                    horizontalAlignment: Text.AlignHCenter
                                    text: RenderRouteService.appsLoaded ? "No matching applications." : "Loading applications..."
                                    font.pixelSize: Theme.fontSizeSmall
                                    color: Theme.surfaceVariantText
                                }

                                Repeater {
                                    model: root.filteredApps

                                    StyledRect {
                                        id: appCard
                                        required property var modelData
                                        readonly property var appData: modelData
                                        width: appsCol.width
                                        height: appRowCol.implicitHeight + Theme.spacingS * 2
                                        radius: Theme.cornerRadius
                                        color: Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency)
                                        border.width: 1
                                        border.color: appData.effective_gpu ? Theme.withAlpha(Theme.primary, 0.45) : Theme.outlineLight

                                        Column {
                                            id: appRowCol
                                            anchors.left: parent.left
                                            anchors.right: parent.right
                                            anchors.top: parent.top
                                            anchors.margins: Theme.spacingS
                                            spacing: Theme.spacingXS

                                            Item {
                                                width: parent.width
                                                height: 22

                                                StyledText {
                                                    anchors.left: parent.left
                                                    anchors.right: appRuleBadge.left
                                                    anchors.rightMargin: Theme.spacingXS
                                                    anchors.verticalCenter: parent.verticalCenter
                                                    text: appCard.appData.name
                                                    font.pixelSize: Theme.fontSizeSmall
                                                    font.weight: Font.Bold
                                                    color: Theme.surfaceText
                                                    elide: Text.ElideRight
                                                }

                                                StyledText {
                                                    id: appRuleBadge
                                                    anchors.right: parent.right
                                                    anchors.verticalCenter: parent.verticalCenter
                                                    text: appCard.appData.effective_gpu ? `Pinned: ${appCard.appData.effective_gpu.toUpperCase()}` : (appCard.appData.binary || "")
                                                    font.pixelSize: Theme.fontSizeSmall - 2
                                                    font.weight: appCard.appData.effective_gpu ? Font.DemiBold : Font.Normal
                                                    color: appCard.appData.effective_gpu ? Theme.primary : Theme.surfaceVariantText
                                                }
                                            }

                                            Flow {
                                                width: parent.width
                                                spacing: Theme.spacingXS

                                                Repeater {
                                                    model: RenderRouteService.gpus

                                                    MiniChipButton {
                                                        required property var modelData
                                                        readonly property string sel: RenderRouteService.selectorForGpu(modelData)
                                                        label: modelData.short_name || sel
                                                        iconName: "play_arrow"
                                                        onClicked: RenderRouteService.launchApp(
                                                            appCard.appData.id,
                                                            sel,
                                                            appCard.appData.name
                                                        )
                                                    }
                                                }

                                                Repeater {
                                                    model: RenderRouteService.gpus

                                                    MiniChipButton {
                                                        required property var modelData
                                                        readonly property string sel: RenderRouteService.selectorForGpu(modelData)
                                                        label: `Pin ${sel.toUpperCase()}`
                                                        iconName: "push_pin"
                                                        highlighted: appCard.appData.effective_gpu === sel
                                                        onClicked: RenderRouteService.setRule(
                                                            appCard.appData.binary || appCard.appData.id,
                                                            sel,
                                                            appCard.appData.id,
                                                            appCard.appData.name,
                                                            RenderRouteService.autoPatchDesktop
                                                        )
                                                    }
                                                }

                                                MiniChipButton {
                                                    visible: Boolean(appCard.appData.effective_gpu)
                                                    label: "Unpin"
                                                    iconName: "close"
                                                    danger: true
                                                    onClicked: RenderRouteService.removeRule(
                                                        appCard.appData.binary || appCard.appData.id,
                                                        appCard.appData.id || ""
                                                    )
                                                }
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }

                    // ================= TAB 2: PERSISTENT RULES =================
                    Column {
                        visible: root.activeTab === 2
                        anchors.fill: parent
                        spacing: Theme.spacingS

                        StyledRect {
                            width: parent.width
                            height: addRuleCol.implicitHeight + Theme.spacingS * 2
                            radius: Theme.cornerRadius
                            color: Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency)
                            border.width: 1
                            border.color: Theme.outlineLight

                            Column {
                                id: addRuleCol
                                anchors.left: parent.left
                                anchors.right: parent.right
                                anchors.top: parent.top
                                anchors.margins: Theme.spacingS
                                spacing: Theme.spacingXS

                                StyledText {
                                    text: "Add Per-Process GPU Rule"
                                    font.pixelSize: Theme.fontSizeSmall
                                    font.weight: Font.DemiBold
                                    color: Theme.surfaceText
                                }

                                Row {
                                    width: parent.width
                                    spacing: Theme.spacingXS

                                    DTextField {
                                        id: ruleTargetField
                                        width: parent.width - addRuleBtn.width - Theme.spacingXS
                                        height: 32
                                        placeholderText: "Process name (e.g. blender, obs, firefox)"
                                        onAccepted: {
                                            RenderRouteService.setRule(text, root.newRuleGpu, "", text, RenderRouteService.autoPatchDesktop);
                                            text = "";
                                        }
                                    }

                                    MiniChipButton {
                                        id: addRuleBtn
                                        height: 32
                                        label: "Pin"
                                        iconName: "add"
                                        highlighted: true
                                        onClicked: {
                                            RenderRouteService.setRule(ruleTargetField.text, root.newRuleGpu, "", ruleTargetField.text, RenderRouteService.autoPatchDesktop);
                                            ruleTargetField.text = "";
                                        }
                                    }
                                }

                                Row {
                                    spacing: Theme.spacingXS

                                    Repeater {
                                        model: RenderRouteService.gpus

                                        MiniChipButton {
                                            required property var modelData
                                            readonly property string sel: RenderRouteService.selectorForGpu(modelData)
                                            label: modelData.short_name || sel.toUpperCase()
                                            iconName: modelData.role === "dgpu" ? "bolt" : "eco"
                                            highlighted: root.newRuleGpu === sel
                                            onClicked: root.newRuleGpu = sel
                                        }
                                    }
                                }
                            }
                        }

                        DFlickable {
                            width: parent.width
                            height: Math.max(80, parent.height - addRuleCol.implicitHeight - Theme.spacingS * 3)
                            contentHeight: rulesCol.implicitHeight
                            clip: true

                            Column {
                                id: rulesCol
                                width: parent.width
                                spacing: Theme.spacingXS

                                StyledText {
                                    visible: RenderRouteService.rules.length === 0
                                    width: parent.width
                                    topPadding: Theme.spacingL
                                    horizontalAlignment: Text.AlignHCenter
                                    text: "No saved rules yet. Pin a process or app above."
                                    font.pixelSize: Theme.fontSizeSmall
                                    color: Theme.surfaceVariantText
                                }

                                Repeater {
                                    model: RenderRouteService.rules

                                    StyledRect {
                                        id: ruleCard
                                        required property var modelData
                                        readonly property var ruleData: modelData
                                        width: rulesCol.width
                                        height: ruleItemCol.implicitHeight + Theme.spacingS * 2
                                        radius: Theme.cornerRadius
                                        color: Theme.withAlpha(Theme.surfaceContainerHigh, Theme.popupTransparency)
                                        border.width: 1
                                        border.color: Theme.outlineLight

                                        Column {
                                            id: ruleItemCol
                                            anchors.left: parent.left
                                            anchors.right: parent.right
                                            anchors.top: parent.top
                                            anchors.margins: Theme.spacingS
                                            spacing: Theme.spacingXS

                                            Item {
                                                width: parent.width
                                                height: 22

                                                StyledText {
                                                    anchors.left: parent.left
                                                    anchors.right: ruleDesktopTag.left
                                                    anchors.verticalCenter: parent.verticalCenter
                                                    text: ruleCard.ruleData.display_name || ruleCard.ruleData.target
                                                    font.pixelSize: Theme.fontSizeSmall
                                                    font.weight: Font.Bold
                                                    color: Theme.surfaceText
                                                    elide: Text.ElideRight
                                                }

                                                StyledText {
                                                    id: ruleDesktopTag
                                                    anchors.right: parent.right
                                                    anchors.verticalCenter: parent.verticalCenter
                                                    text: ruleCard.ruleData.desktop_patched ? "Desktop patched" : ruleCard.ruleData.target
                                                    font.pixelSize: Theme.fontSizeSmall - 2
                                                    color: ruleCard.ruleData.desktop_patched ? Theme.primary : Theme.surfaceVariantText
                                                }
                                            }

                                            Flow {
                                                width: parent.width
                                                spacing: Theme.spacingXS

                                                Repeater {
                                                    model: RenderRouteService.gpus

                                                    MiniChipButton {
                                                        required property var modelData
                                                        readonly property string sel: RenderRouteService.selectorForGpu(modelData)
                                                        label: modelData.short_name || sel.toUpperCase()
                                                        highlighted: ruleCard.ruleData.gpu === sel || ruleCard.ruleData.gpu === modelData.id
                                                        onClicked: RenderRouteService.setRule(
                                                            ruleCard.ruleData.target,
                                                            sel,
                                                            ruleCard.ruleData.desktop_id || "",
                                                            ruleCard.ruleData.display_name || ruleCard.ruleData.target,
                                                            Boolean(ruleCard.ruleData.desktop_patched) || RenderRouteService.autoPatchDesktop
                                                        )
                                                    }
                                                }

                                                MiniChipButton {
                                                    label: "Delete"
                                                    iconName: "delete"
                                                    danger: true
                                                    onClicked: RenderRouteService.removeRule(ruleCard.ruleData.target)
                                                }
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }
}
