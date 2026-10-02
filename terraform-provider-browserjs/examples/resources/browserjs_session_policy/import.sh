# A session's policy is imported by the session's ID. The first apply after
# the import puts the policy in managed-as-code mode if it was not.
terraform import browserjs_session_policy.research s-ab2cd
