# Instructions on how to bring updates from upstream

The most important detail here is that the merge should go up to a certain
release commit.

For example, if the plan is to release version 0.151.0, then the merge should be
up to the upstream commit that is tagged with 0.151.0 in the upstream repository.

This matters for tagging because of conflicts that can arise when bringing the
upstream changes. We cannot tag for release a commit within the merge, only at
or after the merge commit.
