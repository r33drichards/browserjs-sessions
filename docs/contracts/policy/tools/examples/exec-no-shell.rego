# Any program except a shell or another program whose job is to run programs.
# The browser and desktop tools are allowed.
#
# A list of what is denied, so only as good as the list: it stops the obvious
# ways to turn an argument into a command (sh -c, env, xargs), not the many
# programs that can run others with the right arguments (find -exec, git -c,
# make, an interpreter given a script). Its use is to keep every command a
# program and arguments that a person reading the audit trail can read; to
# restrict what runs, list what is allowed (exec-exact-commands.rego).
package browserjs.policy

import rego.v1

launchers := {
	"sh", "bash", "dash", "zsh", "ksh", "fish", "busybox",
	"env", "xargs", "nohup", "setsid", "timeout", "nice", "ionice", "stdbuf", "time", "watch", "script", "sudo", "su", "doas", "chroot", "exec",
	"python", "python3", "node", "perl", "ruby", "lua", "awk", "gawk", "php",
}

allow_tool_call if {
	input.server == "browser"
	input.tool in {"browser_execute", "desktop_execute"}
}

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	only_fields({"bin", "args", "timeout", "cwd"})
	is_string(input.arguments.bin)

	# The name the program is called by, however it was reached:
	# "/bin/sh" and "./sh" are "sh".
	parts := split(input.arguments.bin, "/")
	name := parts[count(parts) - 1]
	not name in launchers
	call_args
}

# The arguments, whether the call gave `args` or left it out (the server
# treats both as none). Undefined when `args` is not an array of strings, which
# denies.
call_args := args if {
	args := object.get(input.arguments, "args", [])
	is_array(args)
	every a in args {
		is_string(a)
	}
}

# Seconds. The server has no maximum of its own.
timeout_within(limit) if {
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= limit
}

# The call has no field this policy has not looked at. `env` among them: it
# can set PATH, which decides what a program name means.
only_fields(known) if {
	is_object(input.arguments)
	count(object.keys(input.arguments) - known) == 0
}

# Reading the output of a command, and stopping one, start nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs", "kill"}
}

