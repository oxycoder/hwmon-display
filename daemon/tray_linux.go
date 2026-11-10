//go:build linux

// Tray icon via the StatusNotifierItem D-Bus protocol (KDE, GNOME with the
// AppIndicator extension — Ubuntu ships it — Xfce, Cinnamon, MATE...) with a
// com.canonical.dbusmenu menu, and login autostart via an XDG autostart
// .desktop entry in ~/.config/autostart.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// reportAutostartError: the XDG autostart entry needs no privileges, so a
// failure is a plain file error; toggleAutostart has already logged it.
func reportAutostartError(error) {}

const (
	sniIface    = "org.kde.StatusNotifierItem"
	sniPath     = dbus.ObjectPath("/StatusNotifierItem")
	menuIface   = "com.canonical.dbusmenu"
	menuPath    = dbus.ObjectPath("/MenuBar")
	watcherName = "org.kde.StatusNotifierWatcher"
	watcherPath = dbus.ObjectPath("/StatusNotifierWatcher")

	idStatus    = 1
	idPanel     = 5
	idAutostart = 3
	idQuit      = 4
	idPorts     = 6   // the "Serial port" submenu
	idPortBase  = 100 // idPortBase+i: the i-th entry of portChoices()
)

type pixmap struct {
	W, H int32
	Data []byte
}

type toolTip struct {
	IconName    string
	IconPixmap  []pixmap
	Title       string
	Description string
}

// menuItem is one dbusmenu layout node: (ia{sv}av).
type menuItem struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

type menuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

type idProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

type linuxTray struct {
	conn  *dbus.Conn
	props *prop.Properties
	mu    sync.Mutex
	rev   uint32
}

var tray *linuxTray

// ---- org.kde.StatusNotifierItem methods (clicks open the menu host-side) ----

func (t *linuxTray) Activate(x, y int32) *dbus.Error          { openPanel(); return nil } // left click (hosts that honour ItemIsMenu show the menu instead)
func (t *linuxTray) SecondaryActivate(x, y int32) *dbus.Error { return nil }
func (t *linuxTray) ContextMenu(x, y int32) *dbus.Error       { return nil }
func (t *linuxTray) Scroll(delta int32, o string) *dbus.Error { return nil }

// ---- com.canonical.dbusmenu -------------------------------------------------

type dbusMenu struct{ t *linuxTray }

func v(x interface{}) dbus.Variant { return dbus.MakeVariant(x) }

func (m *dbusMenu) items() []menuItem {
	auto := int32(0)
	if autostartEnabled() {
		auto = 1
	}
	items := []menuItem{
		{ID: idStatus, Props: map[string]dbus.Variant{"label": v("hwmond: " + getStatus()), "enabled": v(false)}},
		{ID: 2, Props: map[string]dbus.Variant{"type": v("separator")}},
	}
	if panelURL() != "" {
		items = append(items, menuItem{ID: idPanel, Props: map[string]dbus.Variant{"label": v("Open control panel")}})
	}
	sub := menuItem{ID: idPorts, Props: map[string]dbus.Variant{"label": v("Serial port"), "children-display": v("submenu")}, Children: []dbus.Variant{}}
	for i, c := range portChoices() {
		state := int32(0)
		if c.Name == selectedPort() {
			state = 1
		}
		sub.Children = append(sub.Children, dbus.MakeVariant(menuItem{ID: int32(idPortBase + i),
			Props: map[string]dbus.Variant{"label": v(c.Label), "toggle-type": v("radio"), "toggle-state": v(state)}, Children: []dbus.Variant{}}))
	}
	items = append(items, sub)
	return append(items,
		menuItem{ID: idAutostart, Props: map[string]dbus.Variant{"label": v("Start on login"), "toggle-type": v("checkmark"), "toggle-state": v(auto)}},
		menuItem{ID: idQuit, Props: map[string]dbus.Variant{"label": v("Quit")}},
	)
}

// all flattens the menu tree (top level, then submenu entries).
func (m *dbusMenu) all() []menuItem {
	var out []menuItem
	for _, it := range m.items() {
		out = append(out, it)
		for _, c := range it.Children {
			if ch, ok := c.Value().(menuItem); ok {
				out = append(out, ch)
			}
		}
	}
	return out
}

func (m *dbusMenu) find(id int32) (menuItem, bool) {
	for _, it := range m.all() {
		if it.ID == id {
			return it, true
		}
	}
	return menuItem{}, false
}

func (m *dbusMenu) GetLayout(parent int32, depth int32, names []string) (uint32, menuItem, *dbus.Error) {
	m.t.mu.Lock()
	rev := m.t.rev
	m.t.mu.Unlock()
	if parent != 0 {
		it, ok := m.find(parent)
		if !ok {
			return rev, menuItem{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil)
		}
		if depth == 0 {
			it.Children = []dbus.Variant{}
		}
		return rev, it, nil
	}
	root := menuItem{ID: 0, Props: map[string]dbus.Variant{"children-display": v("submenu")}, Children: []dbus.Variant{}}
	if depth != 0 {
		for _, it := range m.items() {
			if depth == 1 {
				it.Children = []dbus.Variant{} // host asked for one level only
			}
			root.Children = append(root.Children, dbus.MakeVariant(it))
		}
	}
	return rev, root, nil
}

func (m *dbusMenu) GetGroupProperties(ids []int32, names []string) ([]idProps, *dbus.Error) {
	out := []idProps{}
	for _, it := range m.all() {
		if len(ids) == 0 {
			out = append(out, idProps{it.ID, it.Props})
			continue
		}
		for _, id := range ids {
			if id == it.ID {
				out = append(out, idProps{it.ID, it.Props})
			}
		}
	}
	return out, nil
}

func (m *dbusMenu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	if it, ok := m.find(id); ok {
		if p, ok := it.Props[name]; ok {
			return p, nil
		}
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil)
}

func (m *dbusMenu) Event(id int32, eventID string, data dbus.Variant, ts uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	switch id {
	case idPanel:
		openPanel()
	case idAutostart:
		toggleAutostart()
		m.t.layoutChanged()
	case idQuit:
		m.t.conn.Close()
		sendStandby()
		os.Exit(0)
	default:
		if i := int(id) - idPortBase; i >= 0 {
			if ch := portChoices(); i < len(ch) {
				selectPort(ch[i].Name)
				m.t.layoutChanged()
			}
		}
	}
	return nil
}

// hookShutdown: shutdown and logout already reach hwmond as SIGTERM (systemd,
// the desktop session). Suspend does not, so listen for logind's
// PrepareForSleep(true) on the system bus and park the display right away;
// the main loop reconnects on the first write after resume.
func hookShutdown() {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return // no logind / no system bus: the firmware's silence timeout still covers it
	}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember("PrepareForSleep")); err != nil {
		conn.Close()
		return
	}
	sig := make(chan *dbus.Signal, 4)
	conn.Signal(sig)
	go func() {
		for s := range sig {
			if len(s.Body) == 1 && s.Body[0] == true {
				sendStandby()
			}
		}
	}()
}

func (m *dbusMenu) EventGroup(events []menuEvent) ([]int32, *dbus.Error) {
	for _, e := range events {
		m.Event(e.ID, e.EventID, e.Data, e.Timestamp)
	}
	return []int32{}, nil
}

func (m *dbusMenu) AboutToShow(id int32) (bool, *dbus.Error) {
	m.t.layoutChanged() // status / toggle may have changed since the host cached it
	return true, nil
}

func (m *dbusMenu) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return ids, []int32{}, nil
}

func (t *linuxTray) layoutChanged() {
	t.mu.Lock()
	t.rev++
	rev := t.rev
	t.mu.Unlock()
	t.conn.Emit(menuPath, menuIface+".LayoutUpdated", rev, int32(0))
}

// ---- setup -------------------------------------------------------------------

func startTray() error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("session bus: %v", err)
	}
	t := &linuxTray{conn: conn, rev: 1}
	menu := &dbusMenu{t: t}

	tip := toolTip{Title: "hwmond", Description: getStatus(), IconPixmap: []pixmap{}}
	t.props, err = prop.Export(conn, sniPath, map[string]map[string]*prop.Prop{
		sniIface: {
			"Category":            {Value: "ApplicationStatus", Emit: prop.EmitTrue},
			"Id":                  {Value: "hwmond", Emit: prop.EmitTrue},
			"Title":               {Value: "hwmond", Emit: prop.EmitTrue},
			"Status":              {Value: "Active", Emit: prop.EmitTrue},
			"WindowId":            {Value: int32(0), Emit: prop.EmitTrue},
			"IconName":            {Value: "utilities-system-monitor", Emit: prop.EmitTrue},
			"IconThemePath":       {Value: "", Emit: prop.EmitTrue},
			"IconPixmap":          {Value: []pixmap{}, Emit: prop.EmitTrue},
			"OverlayIconName":     {Value: "", Emit: prop.EmitTrue},
			"OverlayIconPixmap":   {Value: []pixmap{}, Emit: prop.EmitTrue},
			"AttentionIconName":   {Value: "", Emit: prop.EmitTrue},
			"AttentionIconPixmap": {Value: []pixmap{}, Emit: prop.EmitTrue},
			"AttentionMovieName":  {Value: "", Emit: prop.EmitTrue},
			"ToolTip":             {Value: tip, Emit: prop.EmitTrue},
			"ItemIsMenu":          {Value: true, Emit: prop.EmitTrue},
			"Menu":                {Value: menuPath, Emit: prop.EmitTrue},
		},
	})
	if err != nil {
		return err
	}
	if err := conn.Export(t, sniPath, sniIface); err != nil {
		return err
	}
	sniNode := &introspect.Node{Interfaces: []introspect.Interface{
		introspect.IntrospectData,
		prop.IntrospectData,
		{Name: sniIface, Methods: introspect.Methods(t), Properties: t.props.Introspection(sniIface),
			Signals: []introspect.Signal{{Name: "NewTitle"}, {Name: "NewIcon"}, {Name: "NewToolTip"},
				{Name: "NewStatus", Args: []introspect.Arg{{Name: "status", Type: "s"}}}}},
	}}
	conn.Export(introspect.NewIntrospectable(sniNode), sniPath, "org.freedesktop.DBus.Introspectable")

	menuProps, err := prop.Export(conn, menuPath, map[string]map[string]*prop.Prop{
		menuIface: {
			"Version":       {Value: uint32(3), Emit: prop.EmitTrue},
			"TextDirection": {Value: "ltr", Emit: prop.EmitTrue},
			"Status":        {Value: "normal", Emit: prop.EmitTrue},
			"IconThemePath": {Value: []string{}, Emit: prop.EmitTrue},
		},
	})
	if err != nil {
		return err
	}
	if err := conn.Export(menu, menuPath, menuIface); err != nil {
		return err
	}
	menuNode := &introspect.Node{Interfaces: []introspect.Interface{
		introspect.IntrospectData,
		prop.IntrospectData,
		{Name: menuIface, Methods: introspect.Methods(menu), Properties: menuProps.Introspection(menuIface),
			Signals: []introspect.Signal{
				{Name: "LayoutUpdated", Args: []introspect.Arg{{Name: "revision", Type: "u"}, {Name: "parent", Type: "i"}}},
				{Name: "ItemsPropertiesUpdated", Args: []introspect.Arg{{Name: "updatedProps", Type: "a(ia{sv})"}, {Name: "removedProps", Type: "a(ias)"}}},
			}},
	}}
	conn.Export(introspect.NewIntrospectable(menuNode), menuPath, "org.freedesktop.DBus.Introspectable")

	name := fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	if r, err := conn.RequestName(name, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("request bus name %s: %v", name, err)
	}
	if err := t.register(name); err != nil {
		return err
	}
	tray = t

	// Re-register when the host (watcher) restarts, e.g. after a shell/extension reload.
	conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, watcherName))
	sig := make(chan *dbus.Signal, 8)
	conn.Signal(sig)
	go func() {
		for s := range sig {
			if len(s.Body) == 3 && s.Body[0] == watcherName && s.Body[2] != "" {
				t.register(name)
			}
		}
	}()
	return nil
}

func (t *linuxTray) register(name string) error {
	call := t.conn.Object(watcherName, watcherPath).Call(watcherName+".RegisterStatusNotifierItem", 0, name)
	if call.Err != nil {
		return fmt.Errorf("no StatusNotifierWatcher on the session bus (GNOME needs the AppIndicator extension): %v", call.Err)
	}
	return nil
}

// trayStatusChanged refreshes the tooltip and the status menu entry.
func trayStatusChanged() {
	t := tray
	if t == nil {
		return
	}
	t.props.SetMust(sniIface, "ToolTip", toolTip{Title: "hwmond", Description: getStatus(), IconPixmap: []pixmap{}})
	t.conn.Emit(sniPath, sniIface+".NewToolTip")
	t.layoutChanged()
}

// ---------- login autostart: ~/.config/autostart/hwmond.desktop ----------------

func autostartFile() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "hwmond.desktop"), nil
}

func setAutostart(enable bool) error {
	path, err := autostartFile()
	if err != nil {
		return err
	}
	if !enable {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	cmd := desktopQuote(exe)
	for _, a := range loginArgs {
		cmd += " " + desktopQuote(a)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	entry := "[Desktop Entry]\nType=Application\nName=hwmond\nComment=Hardware monitor display sender\n" +
		"Exec=" + cmd + "\nTerminal=false\nNoDisplay=true\nX-GNOME-Autostart-enabled=true\n"
	return os.WriteFile(path, []byte(entry), 0o644)
}

func autostartEnabled() bool {
	path, err := autostartFile()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// desktopQuote quotes an Exec argument per the Desktop Entry spec.
func desktopQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$`") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}
