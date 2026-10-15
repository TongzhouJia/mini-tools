# x11copy.py —— 不要焦点也能写剪贴板：从 XWayland 那边当 CLIPBOARD 的主人。
#
# Wayland 下写剪贴板得先有键盘焦点（本机 mutter 没有 data-control 协议），
# wl-copy 只能开个看不见的小窗去讨焦点；GNOME 偶尔不给，小窗就挂在 Dock 上
# 等人点，点了才写进去。X11 认剪贴板主人不看焦点，mutter 又会把 X11 的
# CLIPBOARD 同步成整个桌面的剪贴板，所以从这边写：没窗口、不抢焦点、不用点。
#
# 用法跟 wl-copy 一样：文字从 stdin 进；拿到剪贴板就退出 0，后台留一个进程守着
# 内容（X11 的剪贴板也是谁复制谁持有），别人复制了新东西它自己退。
# 拿不到就退出 1，原因写在 stderr。
import os
import signal
import sys

text = sys.stdin.read()

r, w = os.pipe()
if os.fork() > 0:
    os.close(w)
    msg = os.read(r, 4096).decode(errors="replace")
    if msg == "ok":
        sys.exit(0)
    sys.stderr.write(msg or "守剪贴板的进程没起来")
    sys.exit(1)

os.close(r)
os.setsid()  # 脱离调用方的进程组，voice_clip 重启也不会把它连坐杀掉
signal.alarm(8)  # X 连不上之类卡住时别一直挂着

try:
    os.environ["GDK_BACKEND"] = "x11"
    import gi

    gi.require_version("Gtk", "3.0")
    gi.require_version("Gdk", "3.0")
    from gi.repository import Gdk, GLib, Gtk

    if Gdk.Display.get_default() is None:
        raise RuntimeError("连不上 XWayland（DISPLAY=%s）" % os.environ.get("DISPLAY"))

    cb = Gtk.Clipboard.get(Gdk.SELECTION_CLIPBOARD)

    def ours():
        # 别人的窗口 GDK 也会包一层返回（类型是 FOREIGN），不能只看是不是 None
        o = Gdk.selection_owner_get(Gdk.SELECTION_CLIPBOARD)
        return o is not None and o.get_window_type() != Gdk.WindowType.FOREIGN

    cb.set_text(text, -1)
    if not ours():
        raise RuntimeError("X11 没把 CLIPBOARD 给我")
except BaseException as e:
    os.write(w, (str(e) or type(e).__name__).encode())
    os._exit(1)

os.write(w, b"ok")
os.close(w)
signal.alarm(0)
null = os.open(os.devnull, os.O_RDWR)
for fd in (0, 1, 2):
    os.dup2(null, fd)


def check(*_):
    if not ours():
        Gtk.main_quit()
        return False
    return True


cb.connect("owner-change", check)
GLib.timeout_add_seconds(30, check)  # 万一换主人的通知没收到，兜底自己查
Gtk.main()
