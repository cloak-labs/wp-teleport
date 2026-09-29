DROP TABLE IF EXISTS `wp_27_postmeta`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
CREATE TABLE `wp_27_postmeta` (
  `meta_id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `post_id` bigint(20) unsigned NOT NULL DEFAULT 0,
  `meta_key` varchar(255) DEFAULT NULL,
  `meta_value` longtext DEFAULT NULL,
  PRIMARY KEY (`meta_id`),
  KEY `post_id` (`post_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci;
/*!40101 SET character_set_client = @saved_cs_client */;
INSERT INTO `wp_27_postmeta` VALUES (1,10,'_wp_attached_file','2024/05/hero.jpg'),(2,10,'_wp_attachment_metadata','a:3:{s:5:\"width\";i:1920;s:4:\"file\";s:16:\"2024/05/hero.jpg\";s:5:\"sizes\";a:1:{s:5:\"large\";a:2:{s:4:\"file\";s:17:\"hero-1024x576.jpg\";s:3:\"url\";s:58:\"https://wp.localhost/app/uploads/sites/27/2024/05/hero.jpg\";}}}'),(3,11,'cta_link','a:3:{s:5:\"title\";s:10:\"Contact us\";s:3:\"url\";s:33:\"https://wp.localhost/ccc/contact/\";s:6:\"target\";s:0:\"\";}'),(4,11,'_edit_lock','1719000000:1'),(5,12,'rich','<a href=\"https://wp.localhost/ccc/about/\">About</a>\\nIt\'s great'),(6,12,'nullish',NULL);
