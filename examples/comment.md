## 📋 Terraform Plan Summary

**1 of 2 plans have changes:** 1 to delete, 2 to replace, 1 to create, 2 to update, 1 to forget.

<details open>
<summary>📁 <b><code>env/app</code></b> · 1 to delete, 2 to replace, 1 to create, 2 to update, 1 to forget</summary>

[View plan job](https://github.com/example/infra/actions/runs/100/job/201)

**Resources to delete:**

```diff
- terraform_data.delete
```

**Resources to replace:**

```diff
-/+ terraform_data.cbd (create before destroy)
-/+ terraform_data.replace
```

<details>
<summary>Attribute changes</summary>

- `terraform_data.cbd`: replaced because of `triggers_replace`; computed during apply `id`
- `terraform_data.replace`: replaced because of `triggers_replace`; computed during apply `id`

</details>

**Resources to create:**

```diff
+ terraform_data.create
```

**Resources to update:**

```diff
! terraform_data.secret
! terraform_data.update
```

<details>
<summary>Attribute changes</summary>

- `terraform_data.secret`: changed `input`; computed during apply `output`
- `terraform_data.update`: changed `input`; computed during apply `output`

</details>

**Resources to forget (removed from state, not destroyed):**

```diff
# terraform_data.forget
```

</details>

<details>
<summary>✅ <b><code>env/network</code></b> · no changes</summary>

[View plan job](https://github.com/example/infra/actions/runs/100/job/202)

</details>

<sub>Terraform 1.15.9 · Generated 2026-09-29 13:18:39 UTC</sub>
