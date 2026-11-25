# Records docs/demo.gif from the simulated company. Run from the repository root:
#
#   vhs docs/demo.tape

Output docs/demo.gif

Set Shell bash
Set FontFamily "Menlo"
Set FontSize 18
Set Width 1200
Set Height 640
Set Padding 28
Set Framerate 25
Set TypingSpeed 60ms
Set Theme { "name": "WhoToAsk", "background": "#0d0906", "foreground": "#e8e2d9", "cursor": "#e8e2d9", "selection": "#3a2a1c", "black": "#0d0906", "brightBlack": "#8a8178", "red": "#e06c5a", "brightRed": "#e06c5a", "green": "#2bd48c", "brightGreen": "#2bd48c", "yellow": "#e2ae30", "brightYellow": "#e2ae30", "blue": "#6fa8dc", "brightBlue": "#6fa8dc", "magenta": "#c58fd6", "brightMagenta": "#c58fd6", "cyan": "#56c2c2", "brightCyan": "#56c2c2", "white": "#e8e2d9", "brightWhite": "#ffffff" }

Hide
Type@1ms "export PS1='$ ' DEMO=$(mktemp -d)"
Enter
Type@1ms "go build -o $DEMO/whotoask . && $DEMO/whotoask demo --save-index $DEMO/index >/dev/null 2>&1"
Enter
Wait@90s /^\$ ?$/
Type@1ms `whotoask() { "$DEMO/whotoask" --data-dir "$DEMO/index" "$@"; }`
Enter
Type@1ms "clear"
Enter
Show

Sleep 500ms
Type `whotoask ask "payments retry"`
Sleep 400ms
Enter
Sleep 5s
Hide
Type@1ms "clear"
Enter
Show
Type `whotoask ask "kafka consumer lag"`
Sleep 400ms
Enter
Sleep 4s

Hide
Type@1ms `rm -rf "$DEMO"`
Enter
