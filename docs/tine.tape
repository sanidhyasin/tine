# The README's GIF: tine on a Mac, driving the dev server through SSH.
#
# Record it, from the repository's root, with the dev server up:
#
#   go build -o bin/tine ./cmd/tine
#   ./scripts/dev-server.sh ssh-config > .tine-ssh-config
#   vhs docs/tine.tape
#
# It creates, forks and destroys VMs, and leaves none behind. Hidden lines
# that would show end in "tput cuu1; tput el", which erases them.

Output docs/tine.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 600
Set Padding 24
Set TypingSpeed 35ms
Set Theme "Builtin Solarized Dark"

Hide
Type `export TINE_SSH="ssh -F $PWD/.tine-ssh-config" TINE_HOST=ssh://tine-dev PATH="$PWD/bin:$PATH" PS1='$ '; clear`
Enter
Sleep 500ms
Show

Type "# a VM, restored from its image's template:"
Enter
Sleep 500ms
Type "tine create"
Sleep 300ms
Enter
Sleep 3.5s

Hide
Type "vm=$(tine ls | awk 'NR==2{print $1}'); tput cuu1; tput el"
Enter
Sleep 1.5s
Show

Type "# set it up once..."
Enter
Sleep 500ms
Type "tine exec $vm -- sh -c 'echo set up at $(date +%T) > /root/setup.txt'"
Sleep 300ms
Enter
Sleep 2s

Type "# ...then fork it, running, into 10 copies:"
Enter
Sleep 500ms
Type "tine fork $vm -n 10"
Sleep 300ms
Enter
Sleep 5s

Hide
Type "clear"
Enter
Sleep 300ms
Show

Type "tine ls --tree    # the copies share their parent's memory (PSS)"
Sleep 300ms
Enter
Sleep 5s

Hide
Type "c=$(tine ls | awk 'NR==4{print $1}'); tput cuu1; tput el"
Enter
Sleep 1.5s
Show

Type "# each has the set-up, and an identity of its own:"
Enter
Sleep 500ms
Type "tine exec $c -- sh -c 'cat /root/setup.txt; hostname'"
Sleep 300ms
Enter
Sleep 4s

Hide
Type "tine ls | awk 'NR>1{print $1}' | xargs tine rm >/dev/null; clear"
Enter
Sleep 4s
