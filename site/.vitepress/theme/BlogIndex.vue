<script setup lang="ts">
import { withBase } from "vitepress"
import { data as posts } from "../../blog/posts.data"
</script>

<template>
  <ul class="wf-posts">
    <li v-for="post in posts" :key="post.url" class="wf-box">
      <a :href="withBase(post.url)">{{ post.title }}</a>
      <time :datetime="post.date">{{ post.day }}</time>
      <p v-if="post.description">{{ post.description }}</p>
    </li>
  </ul>
  <p v-if="posts.length === 0">No posts yet.</p>
</template>
