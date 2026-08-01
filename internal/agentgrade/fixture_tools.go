package agentgrade

// fixtureToolSpecs is the probe's tool set: a realistic coding-agent
// surface, authored here (see fixture.go for why it is not captured).
//
// The descriptions are long on purpose. A coding agent's tool
// descriptions carry usage rules, preconditions, and failure modes, and
// that prose is a large share of the request the model has to attend to
// while still emitting exactly-formatted calls. Trimming them to one
// line would keep the schema count and lose the pressure.
//
// Schemas nest to depth 7+ (Workflow → phases → items → properties →
// … ) because the reference client reaches 8 and a flat set would never
// exercise the chat template's rendering of nested objects and arrays.
var fixtureToolSpecs = []toolSpec{
	{
		Name: "Read",
		Description: "Read a file from the local filesystem and return its contents with line numbers.\n\n" +
			"- `file_path` must be an absolute path, never a relative one. A relative path is an error " +
			"rather than being resolved against some ambient working directory, because the working " +
			"directory is not stable across calls.\n" +
			"- Reads up to 2000 lines by default. When you already know which region of a large file you " +
			"need, pass `offset` and `limit` and read only that region: reading a whole 20k-line file to " +
			"look at one function wastes context you will need later.\n" +
			"- Output is returned in `cat -n` form, with line numbers starting at 1. The line-number " +
			"prefix is display only — when you later pass a fragment of this content to an edit tool as " +
			"the text to match, strip the prefix first or the match will fail.\n" +
			"- Images (PNG, JPEG, WebP, GIF) are returned visually rather than as bytes. PDFs are read " +
			"page-by-page via `pages`; a PDF over 10 pages requires the parameter. Notebooks (.ipynb) " +
			"come back as cells with their outputs attached.\n" +
			"- Reading a directory, a nonexistent path, or an empty file returns a diagnostic rather " +
			"than content. Do not treat that diagnostic as file content.\n" +
			"- Do NOT re-read a file immediately after editing it just to confirm the edit landed. The " +
			"edit tool fails loudly when it does not apply, so a confirming read only costs context.",
		Schema: obj(map[string]any{
			"file_path": str("Absolute path to the file to read."),
			"offset":    num("Line number to start reading from. Only pass this when the file is too large to read at once."),
			"limit":     num("Number of lines to read. Only pass this when the file is too large to read at once."),
			"pages":     str("Page range for PDF files, e.g. \"1-5\", \"3\", or \"10-20\". Applies to PDFs only; maximum 20 pages per call."),
		}, "file_path"),
	},
	{
		Name: "Write",
		Description: "Write a file to the local filesystem, creating it or replacing it wholesale.\n\n" +
			"Use this for creating a new file, or for fully replacing a file you have already read in " +
			"this session. Overwriting a file you have not read fails: you cannot responsibly replace " +
			"content you have not seen, and the common cause of that call is a mistaken assumption " +
			"about what the file currently holds.\n\n" +
			"For a partial change, use Edit instead. Rewriting a whole file to change three lines " +
			"destroys the diff a reviewer needs and risks silently dropping content you did not " +
			"intend to touch.\n\n" +
			"Prefer editing an existing file over creating a new one. Never create documentation files " +
			"speculatively — write them only when they were asked for.",
		Schema: obj(map[string]any{
			"file_path": str("Absolute path to the file to write."),
			"content":   str("The complete contents to write to the file."),
		}, "file_path", "content"),
	},
	{
		Name: "Edit",
		Description: "Perform an exact string replacement in a file.\n\n" +
			"- You must have read the file in this session before editing it, or the call fails.\n" +
			"- `old_string` must match the file byte-for-byte, including leading indentation, and must " +
			"be unique within the file. A non-unique match is an error rather than an arbitrary choice: " +
			"picking one of several matches silently is how an edit lands in the wrong function.\n" +
			"- When the text you want to change genuinely appears several times and you want all of " +
			"them, pass `replace_all`. When you want exactly one of them, extend `old_string` with " +
			"surrounding context until it is unique.\n" +
			"- Strip the `cat -n` line-number prefix from anything you copied out of a Read result " +
			"before using it as `old_string`.\n" +
			"- `new_string` must differ from `old_string`; a no-op edit is rejected so it cannot be " +
			"mistaken for a successful change.\n" +
			"- Preserve the surrounding indentation exactly. In whitespace-significant languages an " +
			"edit that changes indentation changes meaning.",
		Schema: obj(map[string]any{
			"file_path":   str("Absolute path to the file to modify."),
			"old_string":  str("The exact text to replace, including indentation."),
			"new_string":  str("The text to replace it with. Must differ from old_string."),
			"replace_all": boolp("Replace every occurrence instead of requiring a unique match. Defaults to false."),
		}, "file_path", "old_string", "new_string"),
	},
	{
		Name: "Glob",
		Description: "Find files by path pattern, fast, over any size of codebase.\n\n" +
			"Supports the usual glob syntax, including `**` for a recursive descent: `**/*.go`, " +
			"`internal/**/testdata/*.json`, `cmd/*/main.go`. Results come back sorted by modification " +
			"time, newest first, which is usually the order you want when you are looking for the file " +
			"someone just touched.\n\n" +
			"Use this when you are looking for files BY NAME or location. Use Grep when you are " +
			"looking for files by their CONTENT. When a search needs several rounds of globbing and " +
			"grepping to narrow down, consider delegating the whole search rather than running the " +
			"rounds yourself.\n\n" +
			"`path` scopes the search to a subtree. Omit it to search the whole project; do not pass " +
			"an empty string, and do not pass \"undefined\" or \"null\" — omit the parameter instead.",
		Schema: obj(map[string]any{
			"pattern": str("The glob pattern to match files against."),
			"path":    str("Absolute path of the directory to search in. Omit to search the whole project."),
		}, "pattern"),
	},
	{
		Name: "Grep",
		Description: "Search file contents with a regular expression, built on ripgrep.\n\n" +
			"Always use this rather than invoking `grep` or `rg` through a shell: the output modes " +
			"below are structured, the traversal respects ignore files, and the results are bounded.\n\n" +
			"Output modes:\n" +
			"- `files_with_matches` (default): just the paths. Cheapest; use it to find where to look.\n" +
			"- `content`: the matching lines. Combine with `-A`, `-B`, `-C` for surrounding context and " +
			"`-n` for line numbers.\n" +
			"- `count`: per-file match counts.\n\n" +
			"The pattern is full regex syntax, not a literal — `foo(bar)` is a capture group, and to " +
			"search for a literal brace or parenthesis you must escape it. Filter the file set with " +
			"`glob` (e.g. `*.go`) or `type` (e.g. `go`, `ts`, `py`), and scope the traversal with " +
			"`path`. Set `multiline` when the pattern must span a line boundary; it is slower, so " +
			"leave it off unless the pattern needs it.",
		Schema: obj(map[string]any{
			"pattern":     str("The regular expression to search for in file contents."),
			"path":        str("Absolute path of the file or directory to search. Omit to search the whole project."),
			"glob":        str("Glob filter limiting which files are searched, e.g. \"*.go\" or \"*.{ts,tsx}\"."),
			"type":        str("File-type filter, e.g. \"go\", \"js\", \"rust\". More efficient than an equivalent glob."),
			"output_mode": str("What to return.", "content", "files_with_matches", "count"),
			"-i":          boolp("Case-insensitive search."),
			"-n":          boolp("Show line numbers. Applies to content mode only."),
			"-A":          num("Lines of trailing context. Content mode only."),
			"-B":          num("Lines of leading context. Content mode only."),
			"-C":          num("Lines of context on both sides. Content mode only."),
			"multiline":   boolp("Allow the pattern to match across line boundaries. Slower; leave off unless needed."),
			"head_limit":  num("Truncate output to the first N results."),
		}, "pattern"),
	},
	{
		Name: "Bash",
		Description: "Execute a shell command and return its output.\n\n" +
			"- The working directory persists between calls, but shell state (environment variables, " +
			"functions, aliases) does not: each call starts from a fresh shell initialised from the " +
			"user's profile. Prefer absolute paths over `cd`.\n" +
			"- Avoid using this to read, search, or edit files. There are dedicated tools for each " +
			"(Read, Grep, Glob, Edit) and they are faster, bounded, and produce structured output. " +
			"Reach for the shell when no dedicated tool fits.\n" +
			"- `timeout` is in milliseconds, defaulting to 120000 and capped at 600000.\n" +
			"- `run_in_background` detaches the command so it survives across turns; you are re-invoked " +
			"when it exits. Do not append `&` yourself.\n" +
			"- Command output is shown to you, not reliably to the user. If the user needs to see " +
			"something, say it in your reply.\n" +
			"- Quote any path containing a space. Chain dependent commands with `&&` rather than " +
			"issuing them as separate calls that could interleave.\n\n" +
			"Version control: interactive flags are not supported in this environment. Commit or push " +
			"only when explicitly asked. Never force-push to the default branch, never amend a commit " +
			"you did not create, and never skip hooks unless the user asks you to.",
		Schema: obj(map[string]any{
			"command":           str("The shell command to execute."),
			"description":       str("A clear, concise description of what this command does, in active voice, 5-10 words."),
			"timeout":           num("Timeout in milliseconds. Defaults to 120000, maximum 600000."),
			"run_in_background": boolp("Run the command detached so it keeps running across turns."),
		}, "command"),
	},
	{
		Name: "TodoWrite",
		Description: "Create and maintain a structured task list for the current session.\n\n" +
			"Use this for work that has three or more distinct steps, or that is complex enough that " +
			"losing your place would cost real effort. It gives the user visibility into what you are " +
			"doing and gives you a place to record follow-ups you discover mid-task.\n\n" +
			"Do NOT use it for a single straightforward change, or for purely conversational replies — " +
			"the overhead is not free and a one-item list is noise.\n\n" +
			"Rules that matter: exactly one task may be `in_progress` at a time; mark a task completed " +
			"only when it is genuinely finished (not when tests are still failing, not when the " +
			"implementation is partial); and when you are blocked, keep the task in progress and add a " +
			"new one describing the blocker rather than quietly marking it done.",
		Schema: obj(map[string]any{
			"todos": arr("The complete task list. Always send the whole list, not a delta.", obj(map[string]any{
				"content":    str("The task in imperative form, e.g. \"Run the test suite\"."),
				"status":     str("Current status.", "pending", "in_progress", "completed"),
				"activeForm": str("Present continuous form shown while the task is running, e.g. \"Running the test suite\"."),
			}, "content", "status", "activeForm")),
		}, "todos"),
	},
	{
		Name: "WebFetch",
		Description: "Fetch a URL and process its content with a prompt.\n\n" +
			"The page is retrieved, converted from HTML to markdown, and handed to a fast model " +
			"together with your prompt; you get that model's answer, not the raw page. So the prompt " +
			"should say what you want extracted, not \"summarise this\".\n\n" +
			"- Prefer a dedicated documentation tool when one is available for the host in question; " +
			"it will be more accurate than scraping.\n" +
			"- Upgrade `http://` URLs to `https://`.\n" +
			"- Redirects to a different host are reported rather than followed; reissue the call " +
			"against the new URL yourself so the hop is visible.\n" +
			"- Results are cached briefly, so repeated fetches of the same URL are cheap.\n" +
			"- Fetching sends the URL to an external service. Do not fetch URLs that were assembled " +
			"from private content.",
		Schema: obj(map[string]any{
			"url":    map[string]any{"type": "string", "format": "uri", "description": "The URL to fetch content from."},
			"prompt": str("What to extract from the page."),
		}, "url", "prompt"),
	},
	{
		Name: "WebSearch",
		Description: "Search the web and return formatted results.\n\n" +
			"Useful for information beyond your knowledge cutoff, for current events, and for anything " +
			"where being out of date would produce a wrong answer — library versions, API shapes, " +
			"pricing, availability. When a question is about something that changes, search rather " +
			"than answering from memory.\n\n" +
			"Account for the fact that today's date is later than your training cutoff: treat your " +
			"prior belief about \"the latest version\" as probably stale.\n\n" +
			"`allowed_domains` and `blocked_domains` narrow the result set; they are mutually " +
			"exclusive in practice — passing both rarely does what you want.",
		Schema: obj(map[string]any{
			"query":           str("The search query."),
			"allowed_domains": arr("Only include results from these domains.", str("A domain name.")),
			"blocked_domains": arr("Never include results from these domains.", str("A domain name.")),
		}, "query"),
	},
	{
		Name: "NotebookEdit",
		Description: "Replace, insert, or delete a cell in a Jupyter notebook.\n\n" +
			"`cell_id` identifies the target cell; with `edit_mode: \"insert\"` the new cell is placed " +
			"after it, or at the top of the notebook when omitted. For `insert`, `cell_type` is " +
			"required — there is no sensible default for a cell that does not exist yet. For " +
			"`delete`, `new_source` is ignored.\n\n" +
			"Editing a notebook rewrites its JSON, so an edit that changes cell ids invalidates ids " +
			"you captured earlier in the session; re-read the notebook if you need to make several " +
			"structural edits in a row.",
		Schema: obj(map[string]any{
			"notebook_path": str("Absolute path to the .ipynb file."),
			"cell_id":       str("Id of the cell to operate on."),
			"new_source":    str("The new cell source. Ignored when edit_mode is \"delete\"."),
			"cell_type":     str("The type of the cell. Required when inserting.", "code", "markdown"),
			"edit_mode":     str("The operation to perform.", "replace", "insert", "delete"),
		}, "notebook_path", "new_source"),
	},
	{
		Name: "Agent",
		Description: "Delegate a self-contained, multi-step task to a subagent.\n\n" +
			"Reach for this when answering would mean reading across many files and you only need the " +
			"conclusion rather than the file contents, or when you have independent work that can run " +
			"in parallel. For a single lookup where you already know the file and symbol, search " +
			"directly — delegating a one-line question costs more than answering it.\n\n" +
			"The subagent's report comes back to you, not to the user, so relay whatever matters. " +
			"Each invocation starts fresh with no memory of previous ones; to continue an existing " +
			"subagent with its context intact, message it rather than spawning a new one.\n\n" +
			"Once you have delegated a search, do not also run it yourself — wait for the result. " +
			"Never fabricate or predict a pending subagent's findings; if asked before it returns, " +
			"say it is still running.",
		Schema: obj(map[string]any{
			"description":       str("A short 3-5 word description of the task."),
			"prompt":            str("The full task for the subagent to perform."),
			"subagent_type":     str("Which specialised agent to use. Omit for the general-purpose one."),
			"model":             str("Optional model override for this subagent.", "sonnet", "opus", "haiku"),
			"run_in_background": boolp("Run asynchronously and notify on completion. Defaults to true."),
		}, "description", "prompt"),
	},
	{
		Name: "Workflow",
		Description: "Run a scripted workflow that orchestrates several subagents deterministically.\n\n" +
			"A workflow is for structuring work across many agents: fanning out to cover a large " +
			"surface, gathering independent perspectives before committing to an approach, or taking " +
			"on a migration too large for one context. The script is where that structure lives — " +
			"what fans out, what verifies, what synthesises.\n\n" +
			"Only invoke this when the user has explicitly asked for multi-agent orchestration. " +
			"Workflows can spawn dozens of agents; the scale has to be requested, not inferred from a " +
			"task that would merely benefit from parallelism.\n\n" +
			"The script runs in the background and returns immediately with an id. Prefer pipelining " +
			"items through stages over synchronising every stage: a barrier is only correct when a " +
			"stage genuinely needs all of the previous stage's results together, such as deduplicating " +
			"across the full result set before expensive downstream work.",
		Schema: obj(map[string]any{
			"script": str("The workflow script. Must begin with an exported meta object."),
			"args":   map[string]any{"description": "A value exposed to the script verbatim. Pass arrays and objects as real JSON, not as a JSON-encoded string."},
			"phases": arr("Progress groups, one per phase the script declares.", obj(map[string]any{
				"title":  str("Short phase title, matched exactly against the script's phase calls."),
				"detail": str("One line on what the phase does."),
				"agents": arr("Agents expected in this phase.", obj(map[string]any{
					"label": str("Display label for the agent."),
					"opts": obj(map[string]any{
						"model":     str("Model override for this agent."),
						"effort":    str("Reasoning effort.", "low", "medium", "high"),
						"isolation": str("Set to \"worktree\" to give the agent its own git worktree."),
						"schema": obj(map[string]any{
							"type":       str("The JSON Schema type of the agent's structured output."),
							"properties": map[string]any{"type": "object", "description": "Property definitions for the structured output."},
						}),
					}),
				}, "label")),
			}, "title")),
		}, "script"),
	},
	{
		Name: "TaskCreate",
		Description: "Create a task in the shared task list.\n\n" +
			"Use for work that spans several steps, when the user hands you a list of things to do, or " +
			"when you discover follow-up work mid-task that should not be lost. Give the task a " +
			"specific subject describing the outcome, not the activity.\n\n" +
			"Tasks are created pending. Set dependencies afterwards rather than trying to encode " +
			"ordering in the subject.",
		Schema: obj(map[string]any{
			"subject":     str("A brief, actionable title in imperative form."),
			"description": str("What needs to be done."),
			"activeForm":  str("Present continuous form shown while the task is in progress."),
			"metadata":    map[string]any{"type": "object", "description": "Arbitrary metadata to attach to the task."},
		}, "subject", "description"),
	},
	{
		Name: "TaskUpdate",
		Description: "Update a task: its status, its text, its owner, or its dependencies.\n\n" +
			"Mark a task completed only when it is genuinely finished. If tests are failing, the " +
			"implementation is partial, or you hit an error you did not resolve, leave it in progress " +
			"and create a task describing the blocker. A task list that says \"done\" for work that is " +
			"not done is worse than no task list.\n\n" +
			"Read the task's current state before updating it; another session may have moved it.",
		Schema: obj(map[string]any{
			"taskId":       str("Id of the task to update."),
			"status":       str("New status.", "pending", "in_progress", "completed", "deleted"),
			"subject":      str("New subject."),
			"description":  str("New description."),
			"owner":        str("New owner."),
			"addBlocks":    arr("Task ids that cannot start until this one completes.", str("A task id.")),
			"addBlockedBy": arr("Task ids that must complete before this one can start.", str("A task id.")),
		}, "taskId"),
	},
	{
		Name: "Monitor",
		Description: "Watch a command or condition until it changes, then wake and report.\n\n" +
			"Use this instead of polling in a loop. Polling burns a turn per check and, for anything " +
			"slower than a few seconds, spends most of those turns learning nothing. A monitor sleeps " +
			"until the condition holds and then re-invokes you once.\n\n" +
			"Pick the interval from how fast the watched state actually changes: a build that takes " +
			"eight minutes deserves one check near the end, not forty-eight checks. Set a timeout so " +
			"a condition that never becomes true does not hold the session open indefinitely.\n\n" +
			"Do not use a monitor to wait on work the harness already tracks — you are re-invoked " +
			"automatically when that finishes, so the monitor is pure waste.",
		Schema: obj(map[string]any{
			"command":  str("Shell command whose output is evaluated each interval."),
			"until":    str("Condition that ends the watch. Evaluated against the command's output."),
			"interval": num("Seconds between checks."),
			"timeout":  num("Maximum seconds to keep watching."),
			"reason":   str("One short sentence on what is being waited for. Shown to the user."),
		}, "command", "until"),
	},
	{
		Name: "ExitPlanMode",
		Description: "Signal that planning is complete and request approval to start making changes.\n\n" +
			"Use this only when the task genuinely requires planning an implementation that will write " +
			"code. For research — reading files, searching, building an understanding — there is " +
			"nothing to approve, so do not call it.\n\n" +
			"This tool IS the approval request. Do not also ask \"does this look right?\" in prose, and " +
			"do not use a question tool to ask whether the plan is acceptable: that asks the same " +
			"question twice and the user has to answer both.\n\n" +
			"Resolve open questions BEFORE calling this. A plan that ends with \"I will decide X during " +
			"implementation\" is asking for approval of something the user cannot yet see.",
		Schema: obj(map[string]any{
			"plan": str("The plan to present for approval, as markdown."),
		}),
	},
	{
		Name: "BashOutput",
		Description: "Retrieve output from a running or completed background shell.\n\n" +
			"Returns only output that has not been returned to you before, so calling it " +
			"repeatedly walks forward through the stream rather than re-delivering everything " +
			"from the start. That also means output you do not read is not lost, but it does " +
			"accumulate — a chatty background process left unread for many turns will hand you a " +
			"large block when you finally look.\n\n" +
			"Pass `filter` to keep only the lines matching a regular expression. The filter is " +
			"applied to the new output only, and non-matching lines are discarded rather than " +
			"held back for a later call, so a filter that is too narrow silently loses output.\n\n" +
			"Use this rather than sleeping and re-running the command: the process is already " +
			"running, and re-running it starts a second one.",
		Schema: obj(map[string]any{
			"bash_id": str("Id of the background shell to read from."),
			"filter":  str("Regular expression selecting which new output lines to return."),
		}, "bash_id"),
	},
	{
		Name: "KillShell",
		Description: "Terminate a running background shell by id.\n\n" +
			"Use this when a background command is no longer needed, is producing output nobody " +
			"will read, or has clearly hung. A background shell left running holds its resources " +
			"and keeps producing output for the rest of the session.\n\n" +
			"Termination is not always immediate: a process that ignores the signal, or that has " +
			"spawned children of its own, may take a moment to go away or may leave children " +
			"behind. Check with a status call rather than assuming the kill took effect.",
		Schema: obj(map[string]any{
			"shell_id": str("Id of the background shell to kill."),
		}, "shell_id"),
	},
	{
		Name: "TaskList",
		Description: "List tasks in the shared task list, optionally filtered.\n\n" +
			"Call this before creating a task to avoid duplicating one that already exists, and " +
			"after finishing one to find what to pick up next. The list is shared, so it may have " +
			"changed since you last looked — do not cache it across turns.\n\n" +
			"Filtering by status is the common case: pending work to pick up, in-progress work to " +
			"check on. Filtering by owner tells you what is already claimed.",
		Schema: obj(map[string]any{
			"status": str("Only return tasks in this status.", "pending", "in_progress", "completed"),
			"owner":  str("Only return tasks with this owner."),
			"limit":  num("Maximum number of tasks to return."),
		}),
	},
	{
		Name: "Skill",
		Description: "Invoke a packaged set of instructions for a particular kind of task.\n\n" +
			"A skill is a workflow the user or the project has set up — deployment steps, a review " +
			"checklist, a repository-specific procedure. When the task at hand is one a listed " +
			"skill covers, invoke it first: its instructions replace your default approach for " +
			"that task, and they encode decisions somebody already made deliberately.\n\n" +
			"Use only names from the available list. Do not guess at a name, and do not invent a " +
			"skill because one plausibly ought to exist. Built-in interface commands are not " +
			"skills.\n\n" +
			"If the skill's instructions are already present in the current turn, follow them " +
			"directly rather than invoking it a second time.",
		Schema: obj(map[string]any{
			"skill": str("Exact skill name from the available list, without a leading slash."),
			"args":  str("Optional arguments passed through to the skill."),
		}, "skill"),
	},
	{
		Name: "SlashCommand",
		Description: "Run a user-defined slash command in the current session.\n\n" +
			"Only commands that appear in the available list can be run; a command that is not " +
			"listed does not exist, and inventing one wastes a turn. Some commands take " +
			"arguments, and passing arguments to one that takes none is an error rather than " +
			"being ignored.\n\n" +
			"Do not use this for a command the user has already run in this turn — the " +
			"instructions are loaded, and running it again re-executes any side effects it has.",
		Schema: obj(map[string]any{
			"command": str("The command to run, including its leading slash and any arguments."),
		}, "command"),
	},
	{
		Name: "AskUserQuestion",
		Description: "Ask the user to decide something you cannot resolve yourself.\n\n" +
			"Reserve this for decisions that are genuinely the user's: ones you cannot settle from " +
			"the request, from the code, or from a sensible default. Do not use it for choices " +
			"with a conventional answer, and do not use it for facts you could verify by looking. " +
			"In those cases pick the obvious option, say which one you picked, and continue.\n\n" +
			"Each question needs two to four distinct options. The user can always supply their " +
			"own answer instead, so do not add an \"other\" option yourself. Put your recommended " +
			"option first and mark it as recommended.\n\n" +
			"Do not use this to ask whether a plan is acceptable or whether you should proceed — " +
			"those have their own mechanism, and asking here means the user answers the same " +
			"question twice.",
		Schema: obj(map[string]any{
			"questions": arr("The questions to ask, one to four of them.", obj(map[string]any{
				"question":    str("The complete question, ending in a question mark."),
				"header":      str("A very short label for the question, at most 12 characters."),
				"multiSelect": boolp("Allow more than one option to be selected."),
				"options": arr("The available choices, two to four of them.", obj(map[string]any{
					"label":       str("Display text for the choice, 1-5 words."),
					"description": str("What this choice means and what follows from it."),
					"preview":     str("Optional preview content rendered when the option is focused."),
				}, "label", "description")),
			}, "question", "header", "options", "multiSelect")),
		}, "questions"),
	},
}
