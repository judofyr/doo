# Doo

Doo is a development tool for starting and running services.

## Example

Put this in `~/.config/doo/project.toml`:

```toml
[[targets]]
name = 'postgresql'
runner = 'launchd'
command = '/usr/local/Cellar/postgresql/9.6.3/homebrew.mxcl.postgresql.plist'

[[targets]]
name = 'project-webpack'
cwd = '/projects/app'
command = 'npm run watch'
runner = 'tmux'

[[targets]]
name = 'project-server'
dependencies = ['postgresql']
cwd = '/projects/app'
command = 'npm run'
runner = 'tmux'

[[targets]]
name = 'projectsopen'
dependencies = ['project-webpack', 'project-server']
command = 'open http://localhost:3000/'
```

You can now run:

```
$ doo projects-open
```
