## pachctl create branch

Create a new branch, or update an existing branch, on a repo.

### Synopsis

Create a new branch, or update an existing branch, on a repo, starting a commit on the branch will also create it, so there's often no need to call this.

```
pachctl create branch <repo>@<branch-or-commit> [flags]
```

### Options

```
      --head string           The head of the newly created branch.
  -h, --help                  help for branch
  -p, --provenance []string   The provenance for the branch. format: <repo>@<branch-or-commit> (default [])
```

### Options inherited from parent commands

```
      --no-color   Turn off colors.
  -v, --verbose    Output verbose logs
```

### SEE ALSO

* [pachctl create](pachctl_create.md)	 - Create a new instance of a Pachyderm resource.

###### Auto generated by spf13/cobra on 4-Dec-2019