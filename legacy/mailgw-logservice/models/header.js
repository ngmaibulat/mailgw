'use strict';
const {
  Model
} = require('sequelize');
module.exports = (sequelize, DataTypes) => {
  class Header extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
      // define association here
    }
  }
  Header.init({
    mail_id: DataTypes.INTEGER,
    name: DataTypes.STRING,
    value: DataTypes.TEXT
  }, {
    sequelize,
    modelName: 'Header',
  });
  return Header;
};