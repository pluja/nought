# The README demo; run testenv/demo.sh, which prepares the test data.
Output docs/images/demo.gif

Set Shell "bash"
Set FontSize 15
Set Width 1240
Set Height 720
Set Padding 14
Set Framerate 24
Set Theme "TokyoNight"
Set WindowBar Colorful
Set BorderRadius 10
Set TypingSpeed 80ms

# nou is the dev build (DEMO_BIN), over the demo NOUGHT_HOME set by demo.sh;
# the editor runs without any personal configuration.
Hide
Type `export PATH="$DEMO_BIN:$PATH" EDITOR="nvim --clean" PS1="$ " && clear`
Enter
Sleep 500ms
Show

Type "nou"
Sleep 400ms
Enter
Sleep 3s

# Look around: a newsletter, read as text.
Type "j"
Sleep 1s
Type "j"
Sleep 3s
Type "k"
Sleep 600ms
Type "k"
Sleep 3s

# Reply in the editor, review, send; the undo window counts down.
Type "r"
Sleep 1.5s
Type "/^$"
Enter
Type "ji"
Type "Wonderful, see you on Friday!"
Escape
Sleep 500ms
Type ":wq"
Enter
Sleep 3s
Type "y"
Sleep 8s

# Done with it: archive. The inbox gets smaller.
Type "e"
Sleep 4s
